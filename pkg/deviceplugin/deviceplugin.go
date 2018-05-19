// TODO: remove orhpan interfaces in another thread
// TODO: use prestart container request, no need to wait
// TODO: cleanup if a step fails
// TODO: load networkSpec annotation name from common module
package deviceplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockercli "github.com/docker/docker/client"
	"github.com/golang/glog"
	"github.com/kubevirt/device-plugin-manager/pkg/dpm"
	"github.com/phoracek/kubetron/pkg/spec"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	pluginapi "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1beta1"
)

const (
	nicsPoolSize               = 100
	interfaceNamePrefix        = "nic_"
	letterBytes                = "abcdefghijklmnopqrstuvwxyz0123456789"
	fakeDeviceHostPath         = "/var/run/kubetron-fakedev"
	fakeDeviceGuestPath        = "/tmp/kubetron-fakedev"
	devicepluginCheckpointPath = "/var/lib/kubelet/device-plugins/kubelet_internal_checkpoint"
	networksSpecAnnotationName = "kubetron.network.kubevirt.io/networksSpec"
)

type Lister struct {
	ResourceName      string
	ResourceNamespace string
}

func (l Lister) GetResourceNamespace() string {
	return l.ResourceNamespace
}

// TODO: check if br-int is available
func (l Lister) Discover(pluginListCh chan dpm.PluginNameList) {
	pluginListCh <- dpm.PluginNameList{l.ResourceName}
}

func (l Lister) NewPlugin(bridge string) dpm.PluginInterface {
	return DevicePlugin{}
}

type DevicePlugin struct{}

func (dp DevicePlugin) GetDevicePluginOptions(ctx context.Context, in *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{
		PreStartRequired: false,
	}, nil
}

func (dp *DevicePlugin) Start() error {
	err := createFakeDevice()
	return err
}

func (dp DevicePlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	for {
		var bridgeDevs []*pluginapi.Device
		for i := 0; i < nicsPoolSize; i++ {
			bridgeDevs = append(bridgeDevs, &pluginapi.Device{
				ID:     fmt.Sprintf("nic-%02d", i),
				Health: pluginapi.Healthy,
			})
		}
		s.Send(&pluginapi.ListAndWatchResponse{Devices: bridgeDevs})
		time.Sleep(10 * time.Second)
	}
	return nil
}

// TODO: stop if fails
// TODO: cleanup if fails
func (dp DevicePlugin) Allocate(ctx context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	glog.V(6).Infof("Allocate called")
	responses := pluginapi.AllocateResponse{}

	// TODO: needed?
	for _, _ = range r.ContainerRequests {
		response := pluginapi.ContainerAllocateResponse{}
		responses.ContainerResponses = append(responses.ContainerResponses, &response)
	}

	if len(r.ContainerRequests) != 1 {
		return nil, fmt.Errorf("Allocate request must contain exactly one container request")
	}

	if len(r.ContainerRequests[0].DevicesIDs) != 1 {
		return nil, fmt.Errorf("Allocate request must contain exactly one device")
	}

	allocatedDeviceID := r.ContainerRequests[0].DevicesIDs[0]

	go func() {

		time.Sleep(10 * time.Second)

		podUID, err := findPodUID(allocatedDeviceID)
		if err != nil {
			glog.Errorf("Failed to find pod UID: %v", err)
			return
		}

		pod, err := findPod(podUID)
		if err != nil {
			glog.Errorf("Failed to find pod with given PodUID: %v", err)
			return
		}

		networksSpec, err := buildNetworksSpec(pod)
		if err != nil {
			glog.Errorf("Failed to read networks spec: %v", err)
			return
		}

		containerPid, err := findContainerPid(pod.Name, pod.Namespace)
		if err != nil {
			glog.Errorf("Failed to find container PID: %v", err)
			return
		}

		// TODO: run in parallel, make sure to precreate netns (colission)
		containerName := fmt.Sprintf("k8s_POD_%s_%s", pod.Name, pod.Namespace)
		for _, spec := range *networksSpec {
			if err := exec.Command("attach-pod", containerName, spec.PortName, spec.PortID, spec.MacAddress, strconv.Itoa(containerPid)).Run(); err != nil {
				// TODO: include logs here
				glog.Errorf("attach-pod failed, check logs in respective ds")
			}
		}

	}()

	return &responses, nil
}

func findPodUID(deviceID string) (string, error) {
	checkpointRaw, err := ioutil.ReadFile(devicepluginCheckpointPath)
	if err != nil {
		return "", fmt.Errorf("failed to read device plugin checkpoint file: %v", err)
	}

	var checkpoint map[string]interface{}
	err = json.Unmarshal(checkpointRaw, &checkpoint)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal device plugin checkpoint file: %v", err)
	}

	// TODO: use something smarter to check if found, function
	podUID := ""

EntriesLoop:
	for _, entry := range checkpoint["PodDeviceEntries"].([]interface{}) {
		for _, deviceID := range entry.(map[string]interface{})["DeviceIDs"].([]interface{}) {
			if deviceID.(string) == deviceID {
				podUID = entry.(map[string]interface{})["PodUID"].(string)
				break EntriesLoop
			}
		}
	}

	if podUID == "" {
		return "", fmt.Errorf("failed to find a pod with matching device ID")
	}

	return podUID, nil
}

func findPod(podUID string) (*v1.Pod, error) {
	var thePod *v1.Pod
	podFound := false

	// TODO: keep client in DP struct
	kubeClientConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to obtain kubernetes client config: %v", err)
	}

	kubeclient, err := kubernetes.NewForConfig(kubeClientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to intialize kubernetes client: %v", err)
	}

	pods, err := kubeclient.CoreV1().Pods("").List(metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %v", err)
	}

	for _, pod := range pods.Items {
		if string(pod.UID) == podUID {
			thePod = &pod
			podFound = true
			break
		}
	}

	if !podFound {
		return nil, fmt.Errorf("failed to find a pod with matching ID")
	}

	return thePod, nil
}

func buildNetworksSpec(pod *v1.Pod) (*spec.NetworksSpec, error) {
	var networksSpec *spec.NetworksSpec
	annotations := pod.ObjectMeta.GetAnnotations()
	networksSpecAnnotation, _ := annotations[networksSpecAnnotationName]

	err := json.Unmarshal([]byte(networksSpecAnnotation), &networksSpec)

	return networksSpec, err
}

func findContainerPid(podName, podNamespace string) (int, error) {
	containerName := fmt.Sprintf("k8s_POD_%s_%s", podName, podNamespace)

	// TODO: keep client in DP struct
	dockerclient, err := dockercli.NewEnvClient()
	if err != nil {
		return 0, fmt.Errorf("failed to intialize docker client: %v", err)
	}

	containers, err := dockerclient.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to list docker containers: %v", err)
	}

	containerPid := -1
RetriesLoop:
	for i := 0; i <= 10; i++ {
		for _, container := range containers {
			config, err := dockerclient.ContainerInspect(context.Background(), container.ID)
			if err != nil {
				return 0, fmt.Errorf("failed to inspect docker container: %v", err)
			}

			if strings.Contains(config.Name, containerName) {
				containerPid = config.State.Pid
				break RetriesLoop
			}
		}
		time.Sleep(10 * time.Second)
	}

	if containerPid == -1 {
		return 0, fmt.Errorf("failed to find container PID")
	}

	return containerPid, nil
}

// TODO: use this instead of separate thread during Allocate
func (dp DevicePlugin) PreStartContainer(ctx context.Context, r *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	glog.V(6).Infof("PreStartContainer called")
	var response pluginapi.PreStartContainerResponse
	return &response, nil
}

func createFakeDevice() error {
	_, stat_err := os.Stat(fakeDeviceHostPath)
	if stat_err == nil {
		glog.V(3).Info("Fake block device already exists")
		return nil
	} else if os.IsNotExist(stat_err) {
		glog.V(3).Info("Creating fake block device")
		cmd := exec.Command("mknod", fakeDeviceHostPath, "b", "1", "1")
		err := cmd.Run()
		return err
	} else {
		panic(stat_err)
	}
}
