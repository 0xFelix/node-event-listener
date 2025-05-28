package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tmaxmax/go-sse"
	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
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

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
		log.Printf("shutting down...")
	}()

	ready := &atomic.Bool{}
	listenReadyz(ready)
	listenSSE(ctx, ready, handleEvent(ctx))
}

func listenReadyz(ready *atomic.Bool) {
	http.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK) // Set the HTTP status code to 200 OK
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	go func() {
		server := &http.Server{
			Addr:              "127.0.0.1:8888",
			ReadHeaderTimeout: 3 * time.Second,
		}
		if err := server.ListenAndServe(); err != nil {
			log.Fatalf("http server failed: %v", err)
		}
	}()
}

func listenSSE(ctx context.Context, ready *atomic.Bool, cb sse.EventCallback) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, lookupEnv(ENV_REDFISH_URL), http.NoBody)
	req.SetBasicAuth(lookupEnv(ENV_REDFISH_USER), lookupEnv(ENV_REDFISH_PASS))

	conn := createSSEClient(ready).NewConnection(req)
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

func createSSEClient(ready *atomic.Bool) *sse.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: lookupInsecure()} // #nosec G402
	return &sse.Client{
		HTTPClient: &http.Client{Transport: transport},
		ResponseValidator: func(resp *http.Response) error {
			err := sse.DefaultValidator(resp)
			if err != nil {
				ready.Store(false)
			} else {
				ready.Store(true)
			}
			return err
		},
		OnRetry: func(err error, _ time.Duration) {
			ready.Store(false)
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

func handleEvent(ctx context.Context) sse.EventCallback {
	mIDs := getMessageIDs()
	client := createNodeClient()
	nodeName := lookupEnv(ENV_NODENAME)

	return func(sseEv sse.Event) {
		redfishEv := &struct {
			Events []struct {
				EventType string
				Message   string
				MessageID string
			}
		}{}
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

func createNodeClient() rest.Interface {
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv(ENV_KUBECONFIG))
	if err != nil {
		log.Fatalf("error building kubeconfig: %v", err)
	}
	if err = setConfigDefaults(config); err != nil {
		log.Fatalf("error setting config defaults: %v", err)
	}
	client, err := rest.RESTClientFor(config)
	if err != nil {
		log.Fatalf("error building kubernetes clientset: %v", err)
	}
	return client
}

func setConfigDefaults(config *rest.Config) error {
	gv := k8sv1.SchemeGroupVersion
	config.GroupVersion = &gv
	config.APIPath = "/api"
	scheme := runtime.NewScheme()
	if err := k8sv1.AddToScheme(scheme); err != nil {
		return err
	}
	config.NegotiatedSerializer = serializer.NewCodecFactory(scheme).WithoutConversion()
	return rest.SetKubernetesDefaults(config)
}

func setNodeCondition(ctx context.Context, client rest.Interface, nodeName string) {
	node := &k8sv1.Node{}
	err := client.Get().Resource("nodes").Name(nodeName).Do(ctx).Into(node)
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

	patch := generatePatchPayload(cond, node.Status.Conditions)
	if len(patch) == 0 {
		log.Printf("node condition %s was already set to %s", cond.Type, cond.Status)
		return
	}

	err = client.Patch(types.JSONPatchType).Resource("nodes").Name(nodeName).
		SubResource("status").Body(patch).Do(ctx).Error()
	if err != nil {
		log.Printf("failed to set node condition: %v", err)
	}
}

func generatePatchPayload(newCond *k8sv1.NodeCondition, conditions []k8sv1.NodeCondition) []byte {
	idx := -1
	for i, cond := range conditions {
		if cond.Type == newCond.Type {
			if cond.Status == newCond.Status {
				return nil
			}
			idx = i
			break
		}
	}

	const basePath = "/status/conditions"
	var patches []map[string]any
	if idx >= 0 {
		path := fmt.Sprintf("%s/%d", basePath, idx)
		patches = []map[string]any{
			{
				"op":    "test",
				"path":  path,
				"value": conditions[idx],
			},
			{
				"op":    "replace",
				"path":  path,
				"value": newCond,
			},
		}
	} else {
		patches = []map[string]any{
			{
				"op":    "add",
				"path":  basePath + "/-",
				"value": newCond,
			},
		}
	}

	payload, err := json.Marshal(patches)
	if err != nil {
		log.Fatalf("Error marshaling patches: %v", err)
	}

	return payload
}
