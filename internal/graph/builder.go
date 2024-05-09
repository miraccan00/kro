package graph

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/aws/symphony/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// This package is reponsible of parsing the ResourceGroup field spec and building the Resource Graph.
// It uses the spec.resources field to understand the relationships between resources.
// One of the main challenges is that resources could be of any types, and the relationships between them
// is described using CEL expressions.

// The ResourceGroup node is root of the Resource Graph.
// Other resources can easily consume any spec/metadata/labels/annotations from the ResourceGroup node.
// The resourcegroup node is provided by the user, and it is not created by the controller.

// Other resources are called "children" of the ResourceGroup node.
// They can consume data from the ResourceGroup node, and they can also consume data from other children.
// There two situations where a child can consume data from another child:
// 1. The child is consuming a spec field from another child.
// 2. The child is consuming a status field another child.

type Collection struct {
	ResourceGroup *Resource
	Resources     []*Resource
}

func parseDataWithYQ(raw []byte, path string) (string, error) {
	/* 	cmd := exec.Command("echo", string(raw), "|", "yq", "."+path)
	   	cmd = exec.Command("yq", "."+path) */
	return "", nil
}

func (c *Collection) GetReplaceData() (map[string]string, error) {
	replaceData := make(map[string]string)
	for _, resource := range c.Resources {
		for _, ref := range resource.References {
			switch ref.Type {
			case ReferenceTypeSpec, ReferenceTypeMetadata, ReferenceTypeAnnotation:
				uniData, err := parseDataCELFake(c.ResourceGroup.Raw, ref.Name)
				if err != nil {
					return nil, err
				}
				fmt.Println("USING CEL", ref.Name)
				fmt.Println("ON", string(c.ResourceGroup.Raw))
				fmt.Println("---")
				replaceData[ref.Name] = uniData
			case ReferenceTypeResource:
				target, ok := ref.getTargetResource(c.Resources)
				if !ok {
					return nil, fmt.Errorf("Not found")
				}
				parts := strings.Split(ref.Name, ".")
				parts[0] = "definition"
				jsonPath := strings.Join(parts, ".")

				fmt.Println("USING CEL", jsonPath)
				fmt.Println("ON", string(target.Raw))
				fmt.Println("---")
				uniData, err := parseDataCELFake(target.Raw, jsonPath)
				if err != nil {
					return nil, err
				}
				replaceData[ref.JSONPath] = uniData
			default:
			}
		}
	}
	return replaceData, nil
}

type Resource struct {
	Name           string
	Data           map[string]interface{}
	Raw            []byte
	ReferenceNames []string
	References     []*Reference
	DependsOn      []string
}

func (r *Resource) Unstructured() unstructured.Unstructured {
	return unstructured.Unstructured{
		Object: r.Data,
	}
}

func (r *Resource) Metadata() metav1.ObjectMeta {
	return r.Data["metadata"].(metav1.ObjectMeta)
}

func (r *Resource) ReplaceReferences(data map[string]string) []byte {
	copy := bytes.Clone(r.Raw)
	for _, elem := range data {
		copy = referencesRegex.ReplaceAll(r.Raw, []byte(elem))
	}
	return copy
}

func (r *Resource) WithReplacedReferences(data map[string]string) *Resource {
	return &Resource{
		Name:           r.Name,
		Data:           r.Data,
		Raw:            r.ReplaceReferences(data),
		ReferenceNames: r.ReferenceNames,
		References:     r.References,
		DependsOn:      r.DependsOn,
	}
}

type Builder struct{}

func (b *Builder) Build(rawResourceGroup runtime.RawExtension, resourcegroupResources []*v1alpha1.Resource) (*Collection, error) {
	// Start by walking through the resources and build a map of resources.
	// This map will be used to quickly access a resource by its name.
	resources := make([]*Resource, 0, len(resourcegroupResources))
	for _, resource := range resourcegroupResources {
		var data map[string]interface{}
		err := yaml.Unmarshal(resource.Definition.Raw, &data)
		if err != nil {
			return nil, fmt.Errorf("couldn't parse yaml data from resource %s: %v", resource.Name, err)
		}
		references := extractReferences(resource.Definition.Raw)

		resources = append(resources, &Resource{
			Name:           resource.Name,
			Data:           data,
			Raw:            resource.Definition.Raw,
			ReferenceNames: references,
			DependsOn:      []string{},
			References:     []*Reference{},
		})
	}
	// Validate that names are not duplicated.
	err := validateNamesDuplication(resources)
	if err != nil {
		return nil, err
	}

	// So far we have a map of resources, but we don't know the relationships between them.
	// We need to walk over the variables of each resource and find the relationships.

	for _, resource := range resources {
		for _, ref := range resource.ReferenceNames {
			references, err := buildReference(ref)
			if err != nil {
				return nil, fmt.Errorf("couldn't build variable %s: %v", ref, err)
			}
			resource.References = append(resource.References, references)
			// If the variable is targetting the ResourceGroup node, we don't need to do anything.
			if references.Type == ReferenceTypeResource {
				targetResource, ok := references.getTargetResource(resources)
				if !ok {
					return nil, fmt.Errorf("reference %s is invalid for resource %s", ref, resource.Name)
				}
				if resource.Name == targetResource.Name {
					return nil, fmt.Errorf("resource %s is referencing itself", resource.Name)
				}
				fmt.Println("adding dependency", resource.Name, targetResource.Name)
				resource.DependsOn = append(resource.DependsOn, targetResource.Name)
			}
		}
	}

	// Now just unmarshal the resourcegroup data.
	var resourcegroupData map[string]interface{}
	err = yaml.Unmarshal(rawResourceGroup.Raw, &resourcegroupData)
	if err != nil {
		return nil, fmt.Errorf("couldn't parse yaml data from resourcegroup %v", err)
	}

	// Validate that there is no cyclic dependencies.
	for _, resource := range resources {
		err := detectCyclicDependencies(resource, resources, make(map[string]bool))
		if err != nil {
			return nil, err
		}
	}

	collection := &Collection{
		Resources: resources,
		ResourceGroup: &Resource{
			Name:           "main",
			Data:           resourcegroupData,
			ReferenceNames: []string{},
			DependsOn:      []string{},
			References:     []*Reference{},
		},
	}

	return collection, nil
}

func (c *Builder) GetAllReferences(collection *Collection) []*Reference {
	references := make([]*Reference, 0)
	for _, resource := range collection.Resources {
		references = append(references, resource.References...)
	}
	return references
}

// detectCyclicDependencies is a recursive function that detects cyclic dependencies between resources.
func detectCyclicDependencies(resource *Resource, resources []*Resource, seen map[string]bool) error {
	seen[resource.Name] = true
	for _, dependency := range resource.DependsOn {
		if seen[dependency] {
			return fmt.Errorf("cyclic dependency detected: %s -> %s", resource.Name, dependency)
		}
		for _, r := range resources {
			if r.Name == dependency {
				err := detectCyclicDependencies(r, resources, seen)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateNamesDuplication(resources []*Resource) error {
	seen := make(map[string]int)
	for index, resource := range resources {
		if firstIndex, ok := seen[resource.Name]; ok {
			return fmt.Errorf("resource %s is duplicated: found at %d and %d", resource.Name, firstIndex, index)
		}
		seen[resource.Name] = index
	}
	return nil
}

func isCELExpression(expression string) bool {
	return strings.HasPrefix(expression, "$")
}

func isValidReference(reference string, resourceMap map[string]*Resource) bool {
	if !strings.HasPrefix(reference, "$") {
		return false
	}
	trimed := strings.TrimPrefix(reference, "$")
	parts := strings.Split(trimed, ".")
	if len(parts) < 2 {
		return false
	}
	resourceName := parts[0]
	_, ok := resourceMap[resourceName]
	if !ok {
		return resourceName == "spec" || resourceName == "status"
	}
	return true
}
