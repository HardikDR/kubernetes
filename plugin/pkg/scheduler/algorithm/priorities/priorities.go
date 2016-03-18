/*
Copyright 2014 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package priorities

import (
	"crypto/tls"
	"encoding/json"
	"math"
	//"golang/glog"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/resource"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/plugin/pkg/scheduler/algorithm"
	"k8s.io/kubernetes/plugin/pkg/scheduler/algorithm/predicates"
)

// the unused capacity is calculated on a scale of 0-10
// 0 being the lowest priority and 10 being the highest
func calculateScore(requested int64, capacity int64, node string) int {
	if capacity == 0 {
		return 0
	}
	if requested > capacity {
		glog.V(2).Infof("Combined requested resources %d from existing pods exceeds capacity %d on node %s",
			requested, capacity, node)
		return 0
	}
	return int(((capacity - requested) * 10) / capacity)
}

type ResourceUsage []struct {
	Name     string `json:"name,omitempty"`
	CpuUsage int64  `json:"cpuUsage,omitempty"`
	MemUsage int64  `json:"memUsage,omitempty"`
}

func GetStats_CPU_Mem() (ResourceUsage, error) {
	// Trust Certificates
	url := "https://10.245.1.2/api/v1/proxy/namespaces/kube-system/services/heapster/api/v1/model/nodes/"
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	var timeoutInterval int = 100
	timeout := time.Duration(time.Duration(timeoutInterval) * time.Millisecond)
	client := &http.Client{Transport: tr, Timeout: timeout}
	/* Authenticate */
	req, err := http.NewRequest("GET", url, nil)
	req.SetBasicAuth("vagrant", "vagrant")
	response, err := client.Do(req)

	var Stats ResourceUsage
	if err != nil {
		glog.V(0).Infof("ErrorY: %v\n timeoutInterval:%d\n", err, timeoutInterval)
		if timeoutInterval < 1000 {
			timeoutInterval += 50
		}
	} else {
		defer response.Body.Close()
		body, _ := ioutil.ReadAll(response.Body)
		json.Unmarshal(body, &Stats)
	}
	return Stats, err
}

type IOStats struct {
	Read  int64 `json:"Read,omitempty"`
	Write int64 `json:"Write,omitempty"`
}

type Stats []struct {
	IO    IOStats `json:"stats,omitempty"`
	Major int64   `json:"major,omitempty"`
}

type DiskIOStats struct {
	IO_serviced_bytes Stats `json:"io_serviced,omitempty"`
}

type Interfaces []struct {
	Name     string `json:"name,omitempty"`
	Rx_bytes int64  `json:"rx_bytes,omitempty"`
	Tx_bytes int64  `json:"tx_bytes,omitempty"`
}

type NetworkStats struct {
	Interfaces Interfaces `json:"interfaces,omitempty"`
}

type DiskUsage []struct {
	DiskIO    DiskIOStats  `json:"diskio,omitempty"`
	NetworkIO NetworkStats `json:"network,omitempty"`
}

type MachineStat struct {
	Root DiskUsage `json:"/,omitempty"`
}

func GetStats_Disk_Net(IP string) (MachineStat, error) {
	// Trust Certificates
	url := "http://" + IP + ":4194/api/v2.0/stats"
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	var timeoutInterval int = 100
	//timeout := time.Duration(time.Duration(timeoutInterval) * time.Millisecond)
	client := &http.Client{Transport: tr /*, Timeout: timeout*/}
	/* Authenticate */
	req, err := http.NewRequest("GET", url, nil)
	req.SetBasicAuth("vagrant", "vagrant")
	response, err := client.Do(req)

	var Stats MachineStat
	if err != nil {
		//		glog.V(0).Infof("ErrorY: %v\n timeoutInterval:%d\n", err, timeoutInterval)
		if timeoutInterval < 1000 {
			timeoutInterval += 50
		}
	} else {
		defer response.Body.Close()
		body, _ := ioutil.ReadAll(response.Body)
		json.Unmarshal(body, &Stats)
	}
	return Stats, err
}

// For each of these resources, a pod that doesn't request the resource explicitly
// will be treated as having requested the amount indicated below, for the purpose
// of computing priority only. This ensures that when scheduling zero-request pods, such
// pods will not all be scheduled to the machine with the smallest in-use request,
// and that when scheduling regular pods, such pods will not see zero-request pods as
// consuming no resources whatsoever. We chose these values to be similar to the
// resources that we give to cluster addon pods (#10653). But they are pretty arbitrary.
// As described in #11713, we use request instead of limit to deal with resource requirements.
const defaultMilliCpuRequest int64 = 100             // 0.1 core
const defaultMemoryRequest int64 = 200 * 1024 * 1024 // 200 MB

// TODO: Consider setting default as a fixed fraction of machine capacity (take "capacity api.ResourceList"
// as an additional argument here) rather than using constants
func getNonzeroRequests(requests *api.ResourceList) (int64, int64) {
	var out_millicpu, out_memory int64
	// Override if un-set, but not if explicitly set to zero
	if (*requests.Cpu() == resource.Quantity{}) {
		out_millicpu = defaultMilliCpuRequest
	} else {
		out_millicpu = requests.Cpu().MilliValue()
	}
	// Override if un-set, but not if explicitly set to zero
	if (*requests.Memory() == resource.Quantity{}) {
		out_memory = defaultMemoryRequest
	} else {
		out_memory = requests.Memory().Value()
	}
	return out_millicpu, out_memory
}

// Calculate the resource occupancy on a node.  'node' has information about the resources on the node.
// 'pods' is a list of pods currently scheduled on the node.
func calculateResourceOccupancy(pod *api.Pod, node api.Node, pods []*api.Pod) algorithm.HostPriority {
	totalMilliCPU := int64(0)
	totalMemory := int64(0)
	//The Disk I/O values are computed as the delta of the value from the beginning of the minute to the end of the minute
	// The values for read and write are in KB
	totalDiskIn := int64(0)
	totalDiskOut := int64(0)
	//Network IO is in bytes
	totalNetIn := int64(0)
	totalNetOut := int64(0)

	pref := pod.Annotations["pref"]
	capacityMilliCPU := node.Status.Capacity.Cpu().MilliValue()
	capacityMemory := node.Status.Capacity.Memory().Value()

	for _, existingPod := range pods {
		for _, container := range existingPod.Spec.Containers {
			cpu, memory := getNonzeroRequests(&container.Resources.Requests)
			totalMilliCPU += cpu
			totalMemory += memory
		}
	}
	// Add the resources requested by the current pod being scheduled.
	// This also helps differentiate between differently sized, but empty, nodes.
	for _, container := range pod.Spec.Containers {
		cpu, memory := getNonzeroRequests(&container.Resources.Requests)
		totalMilliCPU += cpu
		totalMemory += memory
	}

	metrics, Err := GetStats_CPU_Mem()

	if Err == nil {
		for _, v := range metrics {
			if v.Name == node.Name {
				totalMilliCPU = v.CpuUsage
				totalMemory = v.MemUsage
				Disk_Network_metrics, error := GetStats_Disk_Net(v.Name)
				if error != nil {
					glog.V(0).Infof("Error in fetching DiskIO and NetworkIO")
				} else {
					totalDiskIn = Disk_Network_metrics.Root[0].DiskIO.IO_serviced_bytes[0].IO.Read
					totalDiskOut = Disk_Network_metrics.Root[0].DiskIO.IO_serviced_bytes[0].IO.Write
					totalNetIn = Disk_Network_metrics.Root[0].NetworkIO.Interfaces[0].Rx_bytes
					totalNetOut = Disk_Network_metrics.Root[0].NetworkIO.Interfaces[0].Tx_bytes
					glog.V(0).Infof(
						"%s : TotalDiskIO = %d/%d , Total NetworkIO = %d/%d ", v.Name, totalDiskIn, totalDiskOut, totalNetIn, totalNetOut,
					)
				}
				glog.V(0).Infof(
					"%s : TotalMilliCPU = %d , TotalMemory = %d , TotalDiskIO = %d/%d , Total NetworkIO = %d/%d , PreferencE: %v , Preferencess: %s", v.Name, totalMilliCPU, totalMemory, totalDiskIn, totalDiskOut, totalNetIn, totalNetOut, pref, pref,
				)
			}
		}
		// Add the resources requested by the current pod being scheduled.
		// This also helps differentiate between differently sized, but empty, nodes.
		for _, container := range pod.Spec.Containers {
			cpu, memory := getNonzeroRequests(&container.Resources.Requests)
			totalMilliCPU += cpu
			totalMemory += memory
		}

	}

	if Err != nil {
		glog.V(0).Infof(" Error while fetching the heapster metrics.")
	}

	cpuScore := calculateScore(totalMilliCPU, capacityMilliCPU, node.Name)
	memoryScore := calculateScore(totalMemory, capacityMemory, node.Name)
	// Disk/Network transfer rates have been hard coded , need to do something about it
	// Assumtions considering generally observed metrics
	// NetworkIO capacity : 1 Gbit/s
	// DiskIO capacity : 70 MB/s
	var diskInScore, diskOutScore, networkInScore, networkOutScore int
	if (totalDiskIn/1024) > 5000 || (totalDiskOut/1024) > 5000 {
		diskInScore = 10
		diskOutScore = 10
	} else {
		diskInScore = calculateScore(totalDiskIn/1024, 5000, node.Name)
		diskOutScore = calculateScore(totalDiskOut/1024, 5000, node.Name)
	}
	diskScore := (diskInScore + diskOutScore) / 2

	if (totalNetIn > 7000000000) || (totalNetOut > 7000000000) {
		networkInScore = 10
		networkOutScore = 10
	} else {
		networkInScore = calculateScore(totalNetIn, 7000000000, node.Name)
		networkOutScore = calculateScore(totalNetOut, 7000000000, node.Name)
	}
	networkScore := (networkInScore + networkOutScore) / 2

	glog.V(0).Infof(
		"%v -> %v: Least Requested Priority, Absolute/Requested: (%d, %d) / (%d, %d) Score: (%d, %d)",
		pod.Name, node.Name,
		totalMilliCPU, totalMemory,
		capacityMilliCPU, capacityMemory,
		cpuScore, memoryScore,
	)

	var Score int

	switch pref {
	case "cpu":
		Score = (1/2)*cpuScore + (1/6)*(memoryScore+diskScore+networkScore)
	case "memory":
		Score = (1/2)*memoryScore + (1/6)*(cpuScore+diskScore+networkScore)
	case "network":
		Score = (1/2)*networkScore + (1/6)*(memoryScore+diskScore+cpuScore)
	case "disk":
		Score = (1/2)*diskScore + (1/6)*(memoryScore+networkScore+networkScore)
	default:
		Score = (cpuScore + memoryScore) / 2
	}

	return algorithm.HostPriority{
		Host:  node.Name,
		Score: Score,
	}
}

// LeastRequestedPriority is a priority function that favors nodes with fewer requested resources.
// It calculates the percentage of memory and CPU requested by pods scheduled on the node, and prioritizes
// based on the minimum of the average of the fraction of requested to capacity.
// Details: cpu((capacity - sum(requested)) * 10 / capacity) + memory((capacity - sum(requested)) * 10 / capacity) / 2
func LeastRequestedPriority(pod *api.Pod, podLister algorithm.PodLister, nodeLister algorithm.NodeLister) (algorithm.HostPriorityList, error) {
	nodes, err := nodeLister.List()
	if err != nil {
		return algorithm.HostPriorityList{}, err
	}
	podsToMachines, err := predicates.MapPodsToMachines(podLister)

	list := algorithm.HostPriorityList{}
	for _, node := range nodes.Items {
		list = append(list, calculateResourceOccupancy(pod, node, podsToMachines[node.Name]))
	}
	return list, nil
}

type NodeLabelPrioritizer struct {
	label    string
	presence bool
}

func NewNodeLabelPriority(label string, presence bool) algorithm.PriorityFunction {
	labelPrioritizer := &NodeLabelPrioritizer{
		label:    label,
		presence: presence,
	}
	return labelPrioritizer.CalculateNodeLabelPriority
}

// CalculateNodeLabelPriority checks whether a particular label exists on a node or not, regardless of its value.
// If presence is true, prioritizes nodes that have the specified label, regardless of value.
// If presence is false, prioritizes nodes that do not have the specified label.
func (n *NodeLabelPrioritizer) CalculateNodeLabelPriority(pod *api.Pod, podLister algorithm.PodLister, nodeLister algorithm.NodeLister) (algorithm.HostPriorityList, error) {
	var score int
	nodes, err := nodeLister.List()
	if err != nil {
		return nil, err
	}

	labeledNodes := map[string]bool{}
	for _, node := range nodes.Items {
		exists := labels.Set(node.Labels).Has(n.label)
		labeledNodes[node.Name] = (exists && n.presence) || (!exists && !n.presence)
	}

	result := []algorithm.HostPriority{}
	//score int - scale of 0-10
	// 0 being the lowest priority and 10 being the highest
	for nodeName, success := range labeledNodes {
		if success {
			score = 10
		} else {
			score = 0
		}
		result = append(result, algorithm.HostPriority{Host: nodeName, Score: score})
	}
	return result, nil
}

// BalancedResourceAllocation favors nodes with balanced resource usage rate.
// BalancedResourceAllocation should **NOT** be used alone, and **MUST** be used together with LeastRequestedPriority.
// It calculates the difference between the cpu and memory fracion of capacity, and prioritizes the host based on how
// close the two metrics are to each other.
// Detail: score = 10 - abs(cpuFraction-memoryFraction)*10. The algorithm is partly inspired by:
// "Wei Huang et al. An Energy Efficient Virtual Machine Placement Algorithm with Balanced Resource Utilization"
func BalancedResourceAllocation(pod *api.Pod, podLister algorithm.PodLister, nodeLister algorithm.NodeLister) (algorithm.HostPriorityList, error) {
	nodes, err := nodeLister.List()
	if err != nil {
		return algorithm.HostPriorityList{}, err
	}
	podsToMachines, err := predicates.MapPodsToMachines(podLister)

	list := algorithm.HostPriorityList{}
	for _, node := range nodes.Items {
		list = append(list, calculateBalancedResourceAllocation(pod, node, podsToMachines[node.Name]))
	}
	return list, nil
}

func calculateBalancedResourceAllocation(pod *api.Pod, node api.Node, pods []*api.Pod) algorithm.HostPriority {
	totalMilliCPU := int64(0)
	totalMemory := int64(0)
	totalDiskIn := int64(0)
	totalDiskOut := int64(0)
	totalNetIn := int64(0)
	totalNetOut := int64(0)
	score := int(0)
	pref := pod.Annotations["pref"]
	for _, existingPod := range pods {
		for _, container := range existingPod.Spec.Containers {
			cpu, memory := getNonzeroRequests(&container.Resources.Requests)
			totalMilliCPU += cpu
			totalMemory += memory
		}
	}
	// Add the resources requested by the current pod being scheduled.
	// This also helps differentiate between differently sized, but empty, nodes.
	for _, container := range pod.Spec.Containers {
		cpu, memory := getNonzeroRequests(&container.Resources.Requests)
		totalMilliCPU += cpu
		totalMemory += memory
	}
	capacityMilliCPU := node.Status.Capacity.Cpu().MilliValue()
	capacityMemory := node.Status.Capacity.Memory().Value()

	metrics, Err := GetStats_CPU_Mem()
	if Err == nil {
		for _, v := range metrics {
			if v.Name == node.Name {
				totalMilliCPU = v.CpuUsage
				totalMemory = v.MemUsage
				Disk_Network_metrics, error := GetStats_Disk_Net(v.Name)
				if error != nil {
					glog.V(0).Infof("Error in fetching DiskIO and NetworkIO")
				} else {
					totalDiskIn = Disk_Network_metrics.Root[0].DiskIO.IO_serviced_bytes[0].IO.Read
					totalDiskOut = Disk_Network_metrics.Root[0].DiskIO.IO_serviced_bytes[0].IO.Write
					totalNetIn = Disk_Network_metrics.Root[0].NetworkIO.Interfaces[0].Rx_bytes
					totalNetOut = Disk_Network_metrics.Root[0].NetworkIO.Interfaces[0].Tx_bytes
					glog.V(0).Infof(
						"%s : TotalDiskIO = %d/%d , Total NetworkIO = %d/%d", v.Name, totalDiskIn, totalDiskOut, totalNetIn, totalNetOut,
					)
					glog.V(0).Infof(
						" %s : TotalMilliCPU = %d , TotalMemory = %d ", v.Name, totalMilliCPU, totalMemory,
					)
				}
				glog.V(0).Infof(
					"%s : TotalMilliCPU = %d , TotalMemory = %d , TotalDiskIO = %d/%d , Total NetworkIO = %d/%d , PreferencE: %v, Preference: %s, ", v.Name, totalMilliCPU, totalMemory, totalDiskIn, totalDiskOut, totalNetIn, totalNetOut, pref, pref,
				)
			}
		}
		// Add the resources requested by the current pod being scheduled.
		// This also helps differentiate between differently sized, but empty, nodes.
		for _, container := range pod.Spec.Containers {
			cpu, memory := getNonzeroRequests(&container.Resources.Requests)
			totalMilliCPU += cpu
			totalMemory += memory
		}

	} else {
		glog.V(0).Infof("Error while fetching the heapster metrics.")

	}

	cpuFraction := fractionOfCapacity(totalMilliCPU, capacityMilliCPU)
	memoryFraction := fractionOfCapacity(totalMemory, capacityMemory)
	if cpuFraction >= 1 || memoryFraction >= 1 {
		// if requested >= capacity, the corresponding host should never be preferrred.
		score = 0
	} else {
		// Upper and lower boundary of difference between cpuFraction and memoryFraction are -1 and 1
		// respectively. Multilying the absolute value of the difference by 10 scales the value to
		// 0-10 with 0 representing well balanced allocation and 10 poorly balanced. Subtracting it from
		// 10 leads to the score which also scales from 0 to 10 while 10 representing well balanced.
		diff := math.Abs(cpuFraction - memoryFraction)
		score = int(10 - diff*10)
	}
	glog.V(10).Infof(
		"%v -> %v: Balanced Resource Allocation, Absolute/Requested: (%d, %d) / (%d, %d) Score: (%d)",
		pod.Name, node.Name,
		totalMilliCPU, totalMemory,
		capacityMilliCPU, capacityMemory,
		score,
	)

	return algorithm.HostPriority{
		Host:  node.Name,
		Score: score,
	}
}

func fractionOfCapacity(requested, capacity int64) float64 {
	if capacity == 0 {
		return 1
	}
	return float64(requested) / float64(capacity)
}
