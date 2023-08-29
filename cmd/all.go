package cmd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/exp/slices"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/spf13/cobra"
)

// allCmd represents the all command
var allCmd = &cobra.Command{
	Use:   "all",
	Short: "Check all conditions of all api-resources",
	Long:  `...`,
	Run: func(cmd *cobra.Command, args []string) {
		runAll(arguments)
	},
}

func init() {
	rootCmd.AddCommand(allCmd)
}

var resourcesToSkip = []string{
	"bindings",
	"tokenreviews",
	"selfsubjectaccessreviews",
	"selfsubjectrulesreviews",
	"localsubjectaccessreviews",
	"subjectaccessreviews",
	"componentstatuses",
}

type Counter struct {
	checkedResources     int32
	checkedConditions    int32
	checkedResourceTypes int32
	startTime            time.Time
}

func (c *Counter) add(o handleResourceTypeOutput) {
	c.checkedResources += o.checkedResources
	c.checkedConditions += o.checkedConditions
	c.checkedResourceTypes += o.checkedResourceTypes
}

func runAll(args Arguments) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	config, err := kubeconfig.ClientConfig()
	if err != nil {
		panic(err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	discoveryClient := clientset.Discovery()

	// Get the list of all API resources available
	serverResources, err := discoveryClient.ServerPreferredResources()
	if err != nil {
		panic(err.Error())
	}

	jobs := make(chan handleResourceTypeInput)
	results := make(chan handleResourceTypeOutput)
	var wg sync.WaitGroup

	// Create workers
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(workerID int32) {
			defer wg.Done()
			for input := range jobs {
				input.workerID = workerID
				results <- handleResourceType(input)
			}
		}(int32(i))
	}

	counter := Counter{startTime: time.Now()}

	go func() {
		for result := range results {
			counter.add(result)
		}
	}()

	for _, group := range serverResources {
		for _, resource := range group.APIResources {
			jobs <- handleResourceTypeInput{
				args:      &args,
				dynClient: dynClient,
				resource:  &resource,
				group:     group,
			}
		}
	}
	close(jobs)
	wg.Wait()
	close(results)
	fmt.Printf("Checked %d conditions of %d resources of %d types. Duration: %s\n",
		counter.checkedConditions, counter.checkedResources, counter.checkedResourceTypes, time.Since(counter.startTime))
}

func containsSlash(s string) bool {
	return len(s) > 0 && s[0] == '/'
}

func printResources(args *Arguments, list *unstructured.UnstructuredList, resourceName string, gvr schema.GroupVersionResource,
	counter *handleResourceTypeOutput, workerID int32) {
	//fmt.Printf("Found %d resources of type %s. group %q version %q resource %q\n", len(list.Items), resourceName, gvr.Group, gvr.Version, gvr.Resource)
	for _, obj := range list.Items {
		counter.checkedResources++
		conditions, _, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
		if err != nil {
			panic(err)
		}
		// Iterate over the conditions slice
		for _, condition := range conditions {
			// Convert each condition to a map[string]interface{}
			conditionMap, ok := condition.(map[string]interface{})
			if !ok {
				fmt.Println("Invalid condition format")
				continue
			}
			counter.checkedConditions++
			// Access the desired fields within the condition map
			// For example, to access the "type" and "status" fields:
			conditionType, _ := conditionMap["type"].(string)
			conditionStatus, _ := conditionMap["status"].(string)
			if conditionToSkip(conditionType) {
				continue
			}
			if conditionTypeHasPositiveMeaning(gvr.Resource, conditionType) && conditionStatus == "True" {
				continue
			}
			if conditionTypeHasNegativeMeaning(conditionType) && conditionStatus == "False" {
				continue
			}
			conditionReason, _ := conditionMap["reason"].(string)
			conditionMessage, _ := conditionMap["message"].(string)
			fmt.Printf("  %s %s %s Condition %s=%s %s %q\n", obj.GetNamespace(), gvr.Resource, obj.GetName(), conditionType, conditionStatus,
				conditionReason, conditionMessage)
		}
	}
	if args.verbose {
		fmt.Printf("    checked %s %s %s workerID=%d\n", gvr.Resource, gvr.Group, gvr.Version, workerID)
	}
}

func conditionToSkip(ct string) bool {
	// Skip conditions which can be True or False, and both values are fine.
	var toSkip = []string{
		"DisruptionAllowed",
		"LoadBalancerAttachedToNetwork",
		"NetworkAttached",
	}
	return slices.Contains(toSkip, ct)
}

var conditionTypesOfResourceWithPositiveMeaning = map[string][]string{
	"hetznerbaremetalmachines": {"AssociateBMHCondition"},
}

func conditionTypeHasPositiveMeaning(resource string, ct string) bool {
	types := conditionTypesOfResourceWithPositiveMeaning[resource]
	if slices.Contains(types, ct) {
		return true
	}

	for _, suffix := range []string{
		"Ready", "Succeeded", "Healthy", "Available", "Approved",
		"Initialized", "PodScheduled", "Complete", "Established",
		"NamesAccepted", "Synced", "Created", "Resized",
		"Progressing", "RemediationAllowed",
		"LoadBalancerAttached",
	} {
		if strings.HasSuffix(ct, suffix) {
			return true
		}
	}
	return false
}

func conditionTypeHasNegativeMeaning(ct string) bool {
	for _, suffix := range []string{
		"Unavailable", "Pressure", "Dangling",
	} {
		if strings.HasSuffix(ct, suffix) {
			return true
		}
	}
	return false
}

type handleResourceTypeInput struct {
	args      *Arguments
	dynClient *dynamic.DynamicClient
	resource  *metav1.APIResource
	group     *metav1.APIResourceList
	workerID  int32
}

type handleResourceTypeOutput struct {
	checkedResourceTypes int32
	checkedResources     int32
	checkedConditions    int32
}

func handleResourceType(input handleResourceTypeInput) handleResourceTypeOutput {
	var output handleResourceTypeOutput

	args := input.args
	resource := input.resource
	dynClient := input.dynClient
	group := input.group
	// Skip subresources like pod/logs, pod/status
	if containsSlash(resource.Name) {
		return output
	}
	if slices.Contains(resourcesToSkip, resource.Name) {
		return output
	}
	gvr := schema.GroupVersionResource{
		Group:    group.GroupVersion,
		Version:  resource.Version,
		Resource: resource.Name,
	}

	// core resource types (pods, deployments, ...) need this.
	// I found his by trial-and-error. Is this correct?
	if gvr.Group == "v1" {
		gvr.Version = gvr.Group
		gvr.Group = ""
	}

	output.checkedResourceTypes++

	if resource.Namespaced {
		list, err := dynClient.Resource(gvr).List(context.TODO(), metav1.ListOptions{})

		if err != nil {
			fmt.Printf("..Error listing %s: %v. group %q version %q resource %q\n", resource.Name, err,
				gvr.Group, gvr.Version, gvr.Resource)
			return output
		}
		printResources(args, list, resource.Name, gvr, &output, input.workerID)

	} else {
		list, err := dynClient.Resource(gvr).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			fmt.Printf("..Error listing %s: %v\n", resource.Name, err)
			return output
		}
		printResources(args, list, resource.Name, gvr, &output, input.workerID)
	}
	return output
}