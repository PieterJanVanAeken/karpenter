/*
Copyright The Kubernetes Authors.

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

package resources

import (
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

// RequestsForPods returns the total resources of a variadic list of podspecs.
func RequestsForPods(pods ...*v1.Pod) v1.ResourceList {
	var resources []v1.ResourceList
	for _, pod := range pods {
		resources = append(resources, Ceiling(&pod.Spec).Requests)
	}
	merged := Merge(resources...)
	merged[v1.ResourcePods] = *resource.NewQuantity(int64(len(pods)), resource.DecimalExponent)
	return merged
}

func RequestsForPodSpecs(podspecs ...*v1.PodSpec) v1.ResourceList {
	var resources []v1.ResourceList
	for _, podspec := range podspecs {
		resources = append(resources, Ceiling(podspec).Requests)
	}
	merged := Merge(resources...)
	merged[v1.ResourcePods] = *resource.NewQuantity(int64(len(podspecs)), resource.DecimalExponent)
	return merged
}

// LimitsForPods returns the total resources of a variadic list of podspecs
func LimitsForPods(pods ...*v1.Pod) v1.ResourceList {
	var resources []v1.ResourceList
	for _, pod := range pods {
		resources = append(resources, Ceiling(&pod.Spec).Limits)
	}
	merged := Merge(resources...)
	merged[v1.ResourcePods] = *resource.NewQuantity(int64(len(pods)), resource.DecimalExponent)
	return merged
}

// Merge the resources from the variadic into a single v1.ResourceList
func Merge(resources ...v1.ResourceList) v1.ResourceList {
	if len(resources) == 0 {
		return v1.ResourceList{}
	}
	result := make(v1.ResourceList, len(resources[0]))
	for _, resourceList := range resources {
		for resourceName, quantity := range resourceList {
			current := result[resourceName]
			current.Add(quantity)
			result[resourceName] = current
		}
	}
	return result
}

// MergeInto sums the resources from src into dest, modifying dest. If you need to repeatedly sum
// multiple resource lists, it allocates less to continually sum into an existing list as opposed to
// constructing a new one for each sum like Merge
func MergeInto(dest v1.ResourceList, src v1.ResourceList) v1.ResourceList {
	if dest == nil {
		sz := len(dest)
		if len(src) > sz {
			sz = len(src)
		}
		dest = make(v1.ResourceList, sz)
	}
	for resourceName, quantity := range src {
		current := dest[resourceName]
		current.Add(quantity)
		dest[resourceName] = current
	}
	return dest
}

func Subtract(lhs, rhs v1.ResourceList) v1.ResourceList {
	result := make(v1.ResourceList, len(lhs))
	for k, v := range lhs {
		result[k] = v.DeepCopy()
	}
	for resourceName := range lhs {
		current := lhs[resourceName]
		if rhsValue, ok := rhs[resourceName]; ok {
			current.Sub(rhsValue)
		}
		result[resourceName] = current
	}
	return result
}

// podSpecRequests calculates the max between the sum of container resources and max of initContainers along with sidecar feature consideration
// inspired from https://github.com/kubernetes/kubernetes/blob/e2afa175e4077d767745246662170acd86affeaf/pkg/api/v1/resource/helpers.go#L96
// https://kubernetes.io/blog/2023/08/25/native-sidecar-containers/
func podSpecRequests(podspec *v1.PodSpec) v1.ResourceList {
	requests := v1.ResourceList{}
	restartableInitContainerReqs := v1.ResourceList{}
	maxInitContainerReqs := v1.ResourceList{}

	for _, container := range podspec.Containers {
		MergeInto(requests, MergeResourceLimitsIntoRequests(container))
	}

	for _, container := range podspec.InitContainers {
		containerReqs := MergeResourceLimitsIntoRequests(container)
		// If the init container's policy is "Always", then we need to add this container's requests to the total requests. We also need to track this container's request as the required requests for other initContainers
		if lo.FromPtr(container.RestartPolicy) == v1.ContainerRestartPolicyAlways {
			MergeInto(requests, containerReqs)
			MergeInto(restartableInitContainerReqs, containerReqs)
			maxInitContainerReqs = MaxResources(maxInitContainerReqs, restartableInitContainerReqs)

		} else {
			// Else, check whether the current container's resource requests combined with the restartableInitContainer requests are greater than the current max
			maxInitContainerReqs = MaxResources(maxInitContainerReqs, Merge(containerReqs, restartableInitContainerReqs))
		}
	}
	// The container's needed requests are the max of all of the container requests combined with native sidecar container requests OR the requests required for a large init containers with native sidecar container requests to run
	requests = MaxResources(requests, maxInitContainerReqs)

	if podspec.Overhead != nil {
		MergeInto(requests, podspec.Overhead)
	}

	return requests
}

// podSpecLimits calculates the max between the sum of container resources and max of initContainers along with sidecar feature consideration
// inspired from https://github.com/kubernetes/kubernetes/blob/e2afa175e4077d767745246662170acd86affeaf/pkg/api/v1/resource/helpers.go#L96
// https://kubernetes.io/blog/2023/08/25/native-sidecar-containers/
func podSpecLimits(podspec *v1.PodSpec) v1.ResourceList {
	limits := v1.ResourceList{}
	restartableInitContainerLimits := v1.ResourceList{}
	maxInitContainerLimits := v1.ResourceList{}

	for _, container := range podspec.Containers {
		MergeInto(limits, container.Resources.Limits)
	}

	for _, container := range podspec.InitContainers {
		// If the init container's policy is "Always", then we need to add this container's limits to the total limits. We also need to track this container's limit as the required limits for other initContainers
		if lo.FromPtr(container.RestartPolicy) == v1.ContainerRestartPolicyAlways {
			MergeInto(limits, container.Resources.Limits)
			MergeInto(restartableInitContainerLimits, container.Resources.Limits)
			maxInitContainerLimits = MaxResources(maxInitContainerLimits, restartableInitContainerLimits)
		} else {
			// Else, check whether the current container's resource limits combined with the restartableInitContainer limits are greater than the current max
			maxInitContainerLimits = MaxResources(maxInitContainerLimits, Merge(container.Resources.Limits, restartableInitContainerLimits))
		}
	}
	// The container's needed limits are the max of all of the container limits combined with native sidecar container limits OR the limits required for a large init containers with native sidecar container limits to run
	limits = MaxResources(limits, maxInitContainerLimits)

	if podspec.Overhead != nil {
		MergeInto(limits, podspec.Overhead)
	}

	return limits
}

func Ceiling(podspec *v1.PodSpec) v1.ResourceRequirements {
	return v1.ResourceRequirements{
		Requests: podSpecRequests(podspec),
		Limits:   podSpecLimits(podspec),
	}
}

// MaxResources returns the maximum quantities for a given list of resources
func MaxResources(resources ...v1.ResourceList) v1.ResourceList {
	resourceList := v1.ResourceList{}
	for _, resource := range resources {
		for resourceName, quantity := range resource {
			if value, ok := resourceList[resourceName]; !ok || quantity.Cmp(value) > 0 {
				resourceList[resourceName] = quantity
			}
		}
	}
	return resourceList
}

// MergeResourceLimitsIntoRequests merges resource limits into requests if no request exists for the given resource
func MergeResourceLimitsIntoRequests(container v1.Container) v1.ResourceList {
	resources := container.Resources.DeepCopy()
	if resources.Requests == nil {
		resources.Requests = v1.ResourceList{}
	}

	if resources.Limits != nil {
		for resourceName, quantity := range resources.Limits {
			if _, ok := resources.Requests[resourceName]; !ok {
				resources.Requests[resourceName] = quantity
			}
		}
	}
	return resources.Requests
}

// Quantity parses the string value into a *Quantity
func Quantity(value string) *resource.Quantity {
	r := resource.MustParse(value)
	return &r
}

// IsZero implements r.IsZero(). This method is provided to make some code a bit cleaner as the Quantity.IsZero() takes
// a pointer receiver and map index expressions aren't addressable, so it can't be called directly.
func IsZero(r resource.Quantity) bool {
	return r.IsZero()
}

func Cmp(lhs resource.Quantity, rhs resource.Quantity) int {
	return lhs.Cmp(rhs)
}

// Fits returns true if the candidate set of resources is less than or equal to the total set of resources.
func Fits(candidate, total v1.ResourceList) bool {
	// If any of the total resource values are negative then the resource will never fit
	for _, quantity := range total {
		if Cmp(*resource.NewScaledQuantity(0, resource.Kilo), quantity) > 0 {
			return false
		}
	}
	for resourceName, quantity := range candidate {
		if Cmp(quantity, total[resourceName]) > 0 {
			return false
		}
	}
	return true
}

// String returns a string version of the resource list suitable for presenting in a log
func String(list v1.ResourceList) string {
	if len(list) == 0 {
		return "{}"
	}
	return pretty.Concise(list)
}

// StringMap returns the string map representation of the resource list
func StringMap(list v1.ResourceList) map[string]string {
	if list == nil {
		return nil
	}
	m := make(map[string]string)
	for k, v := range list {
		m[k.String()] = v.String()
	}
	return m
}
