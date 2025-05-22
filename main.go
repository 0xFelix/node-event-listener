package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/stmcginnis/gofish/redfish"
	"github.com/tmaxmax/go-sse"
	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	ENV_REDFISH_URL      = "REDFISH_URL"
	ENV_REDFISH_USER     = "REDFISH_USER"
	ENV_REDFISH_PASS     = "REDFISH_PASS"
	ENV_REDFISH_INSECURE = "REDFISH_INSECURE"

	ENV_KUBECONFIG  = "KUBECONFIG"
	ENV_NODENAME    = "NODENAME"
	ENV_MESSAGE_IDS = "MESSAGE_IDS"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigChan:
			cancel()
		case <-ctx.Done():
		}
		log.Printf("shutting down...")
	}()

	client := createK8SClient()

	listen(ctx, handleEvent(ctx, client))
}

func createK8SClient() *kubernetes.Clientset {
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv(ENV_KUBECONFIG))
	if err != nil {
		log.Fatalf("error building kubeconfig: %v", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("error building kubernetes clientset: %v", err)
	}
	return client
}

func listen(ctx context.Context, cb sse.EventCallback) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, lookupEnv(ENV_REDFISH_URL), http.NoBody)
	req.SetBasicAuth(lookupEnv(ENV_REDFISH_USER), lookupEnv(ENV_REDFISH_PASS))

	client := createSSEClient()
	conn := client.NewConnection(req)
	conn.SubscribeToAll(cb)

	log.Println("streaming sse events...")
	if err := conn.Connect(); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("sse connection failed: %v", err)
	}
}

func lookupEnv(key string) string {
	val, ok := os.LookupEnv(key)
	if !ok || val == "" {
		log.Fatalf("environment variable %s not set", key)
	}
	return val
}

func createSSEClient() *sse.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: lookupInsecure()} // #nosec G402
	return &sse.Client{
		HTTPClient: &http.Client{Transport: transport},
		OnRetry: func(err error, _ time.Duration) {
			log.Printf("lost connection: %v", err)
			log.Printf("reconnecting...")
		},
	}
}

func lookupInsecure() bool {
	val, ok := os.LookupEnv(ENV_REDFISH_INSECURE)
	if !ok {
		return false
	}
	insecure, err := strconv.ParseBool(val)
	if err != nil {
		log.Fatalf("invalid value %s for environment variable REDFISH_INSECURE", val)
	}
	return insecure
}

func handleEvent(ctx context.Context, client *kubernetes.Clientset) sse.EventCallback {
	mIDs := getMessageIDs()
	nodeName := lookupEnv(ENV_NODENAME)

	return func(sseEv sse.Event) {
		redfishEv := &redfish.Event{}
		if err := json.Unmarshal([]byte(sseEv.Data), &redfishEv); err != nil {
			log.Printf("failed to unmarshal into redfish event: %v", err)
			return
		}
		for _, ev := range redfishEv.Events {
			log.Printf("%s: %s (%s)", ev.EventType, ev.Message, ev.MessageID)

			hasMID := slices.ContainsFunc(mIDs, func(mID string) bool {
				return strings.HasSuffix(ev.MessageID, mID)
			})
			if hasMID {
				log.Printf("setting node Ready condition to False")
				setNodeCondition(ctx, client, nodeName)
			}
		}
	}
}

func getMessageIDs() []string {
	if mIDs := os.Getenv(ENV_MESSAGE_IDS); mIDs != "" {
		return strings.Split(mIDs, ",")
	}

	return []string{
		"ASR0001", // Watchdog reset
	}
}

func setNodeCondition(ctx context.Context, client *kubernetes.Clientset, nodeName string) {
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		log.Printf("failed to get node: %v", err)
		return
	}

	cond := &k8sv1.NodeCondition{
		Type:               k8sv1.NodeReady,
		Status:             k8sv1.ConditionFalse,
		Reason:             "NodeUnhealthy",
		Message:            "Node was deemed unhealthy by BMC",
		LastHeartbeatTime:  metav1.Now(),
		LastTransitionTime: metav1.Now(),
	}

	patch := generatePatchPayload(cond, node.Status)
	if len(patch) == 0 {
		log.Printf("node condition was already set to: %s", cond.Status)
		return
	}

	_, err = client.CoreV1().Nodes().PatchStatus(ctx, nodeName, patch)
	if err != nil {
		log.Printf("failed to set node condition: %v", err)
	}
}

func generatePatchPayload(newCond *k8sv1.NodeCondition, status k8sv1.NodeStatus) []byte {
	found := false
	for i, cond := range status.Conditions {
		if cond.Type == newCond.Type {
			if cond.Status == newCond.Status {
				return nil
			}
			status.Conditions[i] = *newCond
			found = true
			break
		}
	}
	if !found {
		status.Conditions = append(status.Conditions, *newCond)
	}

	patchNode := &k8sv1.Node{
		Status: status,
	}
	payload, err := json.Marshal(patchNode)
	if err != nil {
		log.Fatalf("Error marshaling patch: %v", err)
	}

	return payload
}
