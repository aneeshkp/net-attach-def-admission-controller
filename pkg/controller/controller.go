// Copyright (c) 2019 Network Plumbing Working Group
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/K8sNetworkPlumbingWG/net-attach-def-admission-controller/pkg/localmetrics"
	networkv1 "github.com/K8sNetworkPlumbingWG/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netattachdefClientset "github.com/K8sNetworkPlumbingWG/network-attachment-definition-client/pkg/client/clientset/versioned"
	"github.com/containernetworking/cni/libcni"
	"github.com/golang/glog"
	"github.com/intel/multus-cni/logging"
	"github.com/intel/multus-cni/types"
	api_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	maxRetries    = 5
	podannotation = "k8s.v1.cni.cncf.io/networks"
)

var serverStartTime time.Time

// Event indicate the informerEvent
type Event struct {
	key          string
	namespace    string
	eventType    string
	resourceType string
	name         string
}

// Controller object
type Controller struct {
	clientset    kubernetes.Interface
	queue        workqueue.RateLimitingInterface
	informer     cache.SharedIndexInformer
	nadClientset *netattachdefClientset.Clientset
}

//StartWatching ...  Start prepares watchers and run their controllers, then waits for process termination signals
func StartWatching() {
	var clientset kubernetes.Interface

	/* setup Kubernetes API client */
	config, err := rest.InClusterConfig()
	if err != nil {
		glog.Fatal(err)
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatal(err)
	}
	//get custom clientset for net def, if error ignore
	nadClientset, err := netattachdefClientset.NewForConfig(config)
	if err != nil {
		glog.Fatalf("There was error accessing client set for net attach def %v", err)
	}

	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options meta_v1.ListOptions) (runtime.Object, error) {
				return clientset.CoreV1().Pods("").List(options)
			},
			WatchFunc: func(options meta_v1.ListOptions) (watch.Interface, error) {
				return clientset.CoreV1().Pods("").Watch(options)
			},
		},
		&api_v1.Pod{},
		0, //Skip resync
		cache.Indexers{},
	)

	c := newResourceController(clientset, nadClientset, informer, "pod")
	stopCh := make(chan struct{})
	defer close(stopCh)
	go c.Run(stopCh)

	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)
	signal.Notify(sigterm, syscall.SIGINT)
	<-sigterm
}

func newResourceController(client kubernetes.Interface, nadClient *netattachdefClientset.Clientset, informer cache.SharedIndexInformer, resourceType string) *Controller {
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	var newEvent Event
	var err error
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			newEvent.key, err = cache.MetaNamespaceKeyFunc(obj)
			newEvent.namespace, _, err = cache.SplitMetaNamespaceKey(newEvent.key)
			newEvent.eventType = "create"
			newEvent.resourceType = resourceType
			if err == nil {
				queue.Add(newEvent)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			newEvent.key, err = cache.MetaNamespaceKeyFunc(old)
			newEvent.eventType = "update"
			newEvent.resourceType = resourceType
			//dont add to the queue , no need to process

		},
		DeleteFunc: func(obj interface{}) {
			newEvent.key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			newEvent.namespace, _, err = cache.SplitMetaNamespaceKey(newEvent.key)
			newEvent.eventType = "delete"
			newEvent.resourceType = resourceType
			pod := obj.(meta_v1.Object)
			if pod != nil {
				if configName, ok := pod.GetAnnotations()[podannotation]; ok {
					newEvent.resourceType = "net-def-attach"
					newEvent.name = configName
				}
			}
			if err == nil {
				queue.Add(newEvent)
			}

		},
	})

	return &Controller{
		clientset:    client,
		nadClientset: nadClient,
		informer:     informer,
		queue:        queue,
	}
}

// Run starts the kubewatch controller
func (c *Controller) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	c.initNetDefCount()

	glog.Info("Starting Net-Def-Attach admission controller")

	serverStartTime = time.Now().Local()

	go c.informer.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.HasSynced) {
		utilruntime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	glog.Info("Net-Def-Attach controller synced and ready")

	wait.Until(c.runWorker, time.Second, stopCh)
}

// HasSynced is required for the cache.Controller interface.
func (c *Controller) HasSynced() bool {
	return c.informer.HasSynced()
}

// LastSyncResourceVersion is required for the cache.Controller interface.
func (c *Controller) LastSyncResourceVersion() string {
	return c.informer.LastSyncResourceVersion()
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
		// continue looping
	}
}

func (c *Controller) processNextItem() bool {
	newEvent, quit := c.queue.Get()

	if quit {
		return false
	}
	defer c.queue.Done(newEvent)
	err := c.processItem(newEvent.(Event))
	if err == nil {
		// No error, reset the ratelimit counters
		c.queue.Forget(newEvent)
	} else if c.queue.NumRequeues(newEvent) < maxRetries {
		glog.Errorf("Error processing %s (will retry): %v", newEvent.(Event).key, err)
		c.queue.AddRateLimited(newEvent)
	} else {
		// err != nil and too many retries(-
		glog.Errorf("Error processing %s (giving up): %v", newEvent.(Event).key, err)
		c.queue.Forget(newEvent)
		utilruntime.HandleError(err)
	}

	return true
}

func (c *Controller) processItem(newEvent Event) error {
	obj, _, err := c.informer.GetIndexer().GetByKey(newEvent.key)
	if err != nil {
		return fmt.Errorf("Error fetching object with key %s from store: %v", newEvent.key, err)
	}

	// process events based on its type
	switch newEvent.eventType {
	case "create":
		// compare CreationTimestamp and serverStartTime and alert only on latest events
		// Could be Replaced by using Delta or DeltaFIFO
		pod := obj.(meta_v1.Object)
		if name, ok := pod.GetAnnotations()[podannotation]; ok {
			newEvent.name = name
			c.updateMetrics(newEvent, 1.0)

		}
		return nil
	case "delete":
		//too late if pod is already deleted to read
		if newEvent.resourceType == "net-def-attach" {
			c.updateMetrics(newEvent, -1.0)
		}
		return nil
	}
	return nil
}

// find crd by name
func (c *Controller) getCrdByName(name string, namespace string) (*networkv1.NetworkAttachmentDefinition, error) {
	netAttachDef, err := c.nadClientset.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("Failed to locate network attachment definition %s/%s", namespace, name)
	}
	return netAttachDef, nil
}

func (c *Controller) getConfigTypes(crd *networkv1.NetworkAttachmentDefinition) []string {
	var confBytes []byte
	var configTypes []string
	set := make(map[string]struct{})

	if crd.Spec.Config != "" {
		// try to unmarshal config into NetworkConfig or NetworkConfigList
		//  using actual code from libcni - if succesful, it means that the config
		//  will be accepted by CNI itself as well
		confBytes = []byte(crd.Spec.Config)
		networkConfigList, err := libcni.ConfListFromBytes(confBytes)

		if err != nil { // if error check for config
			networkConfi, err := libcni.ConfFromBytes(confBytes)
			if err == nil {
				if _, found := set[networkConfi.Network.Type]; !found {
					set[networkConfi.Network.Type] = struct{}{}
				}
			}
		} else {
			for _, plugin := range networkConfigList.Plugins {
				if _, found := set[plugin.Network.Type]; !found {
					set[plugin.Network.Type] = struct{}{}
				}
			}
		}
		// Convert map to slice of keys.
		for key := range set {
			configTypes = append(configTypes, key)
		}

	}

	return configTypes
}

//keeps the count accurate when the pod gets created
func (c *Controller) initNetDefCount() {
	// intialize  netdef metrics values.
	list, err := c.nadClientset.K8sCniCncfIoV1().NetworkAttachmentDefinitions("").List(metav1.ListOptions{})
	if err == nil {
		localmetrics.UpdateNetAttachDefCRMetrics(float64(len(list.Items)))
	} else {
		glog.Infof("Error getting initial net def count %v", err)
	}
}

func (c *Controller) updateMetrics(newEvent Event, x float64) {
	set := make(map[string]struct{})
	var configTypes []string
	joinedNetworkTypes := "other"
	networks, err := c.parsePodNetworkAnnotation(newEvent.name, newEvent.namespace)
	if err != nil {
		localmetrics.UpdateNetDefAttachInstanceMetrics(joinedNetworkTypes, x)
		glog.Infof("Error reading pod annotation %s", err)
		return
	}
	for _, val := range networks {
		if crd, ok := c.getCrdByName(val.Name, val.Namespace); ok == nil {
			for _, val := range c.getConfigTypes(crd) {
				if _, found := set[val]; !found {
					set[val] = struct{}{}
				}
			}
		}
	}
	for key := range set {
		configTypes = append(configTypes, key)
	}
	if len(configTypes) > 0 {
		joinedNetworkTypes = strings.Join(configTypes, ",")
	}

	localmetrics.UpdateNetDefAttachInstanceMetrics(joinedNetworkTypes, x)
	return

}

func (c *Controller) parsePodNetworkAnnotation(podNetworks, defaultNamespace string) ([]*types.NetworkSelectionElement, error) {
	var networks []*types.NetworkSelectionElement

	if podNetworks == "" {
		return nil, fmt.Errorf("parsePodNetworkAnnotation: pod annotation not having \"network\" as key, refer Multus README.md for the usage guide")
	}

	if strings.IndexAny(podNetworks, "[{\"") >= 0 {
		if err := json.Unmarshal([]byte(podNetworks), &networks); err != nil {
			return nil, fmt.Errorf("parsePodNetworkAnnotation: failed to parse pod Network Attachment Selection Annotation JSON format: %v", err)
		}
	} else {
		// Comma-delimited list of network attachment object names
		for _, item := range strings.Split(podNetworks, ",") {
			// Remove leading and trailing whitespace.
			item = strings.TrimSpace(item)

			// Parse network name (i.e. <namespace>/<network name>@<ifname>)
			netNsName, networkName, netIfName, err := c.parsePodNetworkObjectName(item)
			if err != nil {
				return nil, fmt.Errorf("parsePodNetworkAnnotation: %v", err)
			}

			networks = append(networks, &types.NetworkSelectionElement{
				Name:             networkName,
				Namespace:        netNsName,
				InterfaceRequest: netIfName,
			})
		}
	}

	for _, net := range networks {
		if net.Namespace == "" {
			net.Namespace = defaultNamespace
		}
	}

	return networks, nil
}

func (c *Controller) parsePodNetworkObjectName(podnetwork string) (string, string, string, error) {
	var netNsName string
	var netIfName string
	var networkName string

	slashItems := strings.Split(podnetwork, "/")
	if len(slashItems) == 2 {
		netNsName = strings.TrimSpace(slashItems[0])
		networkName = slashItems[1]
	} else if len(slashItems) == 1 {
		networkName = slashItems[0]
	} else {
		return "", "", "", fmt.Errorf("parsePodNetworkObjectName: Invalid network object (failed at '/')")
	}

	atItems := strings.Split(networkName, "@")
	networkName = strings.TrimSpace(atItems[0])
	if len(atItems) == 2 {
		netIfName = strings.TrimSpace(atItems[1])
	} else if len(atItems) != 1 {
		return "", "", "", fmt.Errorf("parsePodNetworkObjectName: Invalid network object (failed at '@')")
	}

	// Check and see if each item matches the specification for valid attachment name.
	// "Valid attachment names must be comprised of units of the DNS-1123 label format"
	// [a-z0-9]([-a-z0-9]*[a-z0-9])?
	// And we allow at (@), and forward slash (/) (units separated by commas)
	// It must start and end alphanumerically.
	allItems := []string{netNsName, networkName, netIfName}
	for i := range allItems {
		matched, _ := regexp.MatchString("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", allItems[i])
		if !matched && len([]rune(allItems[i])) > 0 {
			return "", "", "", logging.Errorf(fmt.Sprintf("parsePodNetworkObjectName: Failed to parse: one or more items did not match comma-delimited format (must consist of lower case alphanumeric characters). Must start and end with an alphanumeric character), mismatch @ '%v'", allItems[i]))
		}
	}

	return netNsName, networkName, netIfName, nil
}
