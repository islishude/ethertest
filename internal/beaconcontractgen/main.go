package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v2"
)

const specPath = "specs/upstream/beacon-api-subset.yaml"

type document struct {
	Paths      map[string]pathItem `yaml:"paths"`
	Components struct {
		Parameters map[string]parameter `yaml:"parameters"`
		Schemas    map[string]schema    `yaml:"schemas"`
	} `yaml:"components"`
}

type pathItem struct {
	Get *operation `yaml:"get"`
}

type operation struct {
	Parameters []parameter `yaml:"parameters"`
}

type parameter struct {
	Ref      string `yaml:"$ref"`
	Name     string `yaml:"name"`
	In       string `yaml:"in"`
	Required bool   `yaml:"required"`
	Schema   schema `yaml:"schema"`
}

type schema struct {
	Type       string            `yaml:"type"`
	Required   []string          `yaml:"required"`
	Properties map[string]schema `yaml:"properties"`
	Items      *schema           `yaml:"items"`
	Enum       []string          `yaml:"enum"`
}

func main() {
	check := flag.Bool("check", false, "fail if generated output is stale")
	flag.Parse()
	spec, err := os.ReadFile(specPath)
	if err != nil {
		panic(err)
	}
	var contract document
	if err := yaml.Unmarshal(spec, &contract); err != nil {
		panic(fmt.Errorf("parse vendored Beacon contract: %w", err))
	}
	requiredPaths := []string{
		"/eth/v2/beacon/blocks/{block_id}",
		"/eth/v1/debug/beacon/data_column_sidecars/{block_id}",
		"/eth/v1/beacon/states/{state_id}/validators",
		"/eth/v1/beacon/states/{state_id}/validator_balances",
		"/eth/v1/events",
	}
	for _, path := range requiredPaths {
		if item, exists := contract.Paths[path]; !exists || item.Get == nil {
			panic(fmt.Sprintf("vendored Beacon contract is missing GET %s", path))
		}
	}
	for _, removed := range []string{
		"/eth/v1/beacon/blocks/{block_id}",
		"/eth/v1/beacon/data_column_sidecars/{block_id}",
	} {
		if _, exists := contract.Paths[removed]; exists {
			panic(fmt.Sprintf("removed Beacon path returned to contract: %s", removed))
		}
	}
	for name, required := range map[string][]string{
		"DataEnvelope":      {"data"},
		"ErrorMessage":      {"code", "message"},
		"Response":          {"execution_optimistic", "finalized", "data"},
		"VersionedResponse": {"version", "execution_optimistic", "finalized", "data"},
	} {
		value, exists := contract.Components.Schemas[name]
		if !exists || !sameStrings(value.Required, required) {
			panic(fmt.Sprintf("Beacon schema %s has required fields %v, want %v", name, value.Required, required))
		}
	}

	var output bytes.Buffer
	fmt.Fprintf(&output, "// Code generated from %s; DO NOT EDIT.\n\npackage ethertest\n\n", specPath)
	writeRequest(&output, "BeaconBlockRequest", requestParameters(contract, "/eth/v2/beacon/blocks/{block_id}"))
	writeRequest(&output, "BeaconDataColumnSidecarsRequest", requestParameters(contract, "/eth/v1/debug/beacon/data_column_sidecars/{block_id}"))
	writeRequest(&output, "BeaconValidatorsRequest", requestParameters(contract, "/eth/v1/beacon/states/{state_id}/validators"))
	writeRequest(&output, "BeaconValidatorBalancesRequest", requestParameters(contract, "/eth/v1/beacon/states/{state_id}/validator_balances"))
	eventParameters := requestParameters(contract, "/eth/v1/events")
	writeRequest(&output, "BeaconEventsRequest", eventParameters)

	writeSchema(&output, contract, "BeaconErrorMessage", "ErrorMessage", false)
	writeSchema(&output, contract, "BeaconDataEnvelope", "DataEnvelope", true)
	writeSchema(&output, contract, "BeaconResponse", "Response", true)
	writeSchema(&output, contract, "BeaconVersionedResponse", "VersionedResponse", true)

	writeEnumSet(&output, "beaconGeneratedEventTopics", parameterByName(eventParameters, "topics").Schema.Items.Enum)
	statuses := contract.Components.Parameters["ValidatorStatuses"].Schema.Items
	if statuses == nil || len(statuses.Enum) == 0 {
		panic("ValidatorStatuses must contain a locked enum")
	}
	writeEnumSet(&output, "beaconGeneratedValidatorStatuses", statuses.Enum)

	formatted, err := format.Source(output.Bytes())
	if err != nil {
		panic(fmt.Errorf("format generated Beacon contract: %w\n%s", err, output.Bytes()))
	}
	if *check {
		current, err := os.ReadFile("beacon_contract_generated.go")
		if err != nil {
			panic(err)
		}
		if !bytes.Equal(current, formatted) {
			panic("beacon_contract_generated.go is stale; run go generate ./...")
		}
		return
	}
	if err := os.WriteFile("beacon_contract_generated.go", formatted, 0o644); err != nil {
		panic(err)
	}
}

func requestParameters(contract document, path string) []parameter {
	parameters := contract.Paths[path].Get.Parameters
	resolved := make([]parameter, len(parameters))
	for index, value := range parameters {
		if value.Ref == "" {
			resolved[index] = value
			continue
		}
		const prefix = "#/components/parameters/"
		if !strings.HasPrefix(value.Ref, prefix) {
			panic(fmt.Sprintf("unsupported parameter reference %q", value.Ref))
		}
		name := strings.TrimPrefix(value.Ref, prefix)
		resolvedValue, exists := contract.Components.Parameters[name]
		if !exists {
			panic(fmt.Sprintf("parameter reference %q is missing", value.Ref))
		}
		resolved[index] = resolvedValue
	}
	return resolved
}

func writeRequest(output *bytes.Buffer, name string, parameters []parameter) {
	fmt.Fprintf(output, "// %s is generated from the locked path and query parameters.\ntype %s struct {\n", name, name)
	for _, value := range parameters {
		fieldName := map[string]string{
			"block_id": "BlockID", "state_id": "StateID", "id": "IDs",
			"status": "Statuses", "indices": "Indices", "topics": "Topics",
		}[value.Name]
		if fieldName == "" || (value.In != "path" && value.In != "query") {
			panic(fmt.Sprintf("unsupported request parameter %s in %s", value.Name, value.In))
		}
		fieldType := "string"
		if value.Schema.Type == "array" {
			fieldType = "[]string"
		} else if value.Schema.Type != "string" {
			panic(fmt.Sprintf("unsupported request schema for %s: %s", value.Name, value.Schema.Type))
		}
		fmt.Fprintf(output, "\t%s %s `%s:\"%s\"`\n", fieldName, fieldType, value.In, value.Name)
	}
	output.WriteString("}\n\n")
}

func writeSchema(output *bytes.Buffer, contract document, goName, schemaName string, generic bool) {
	value := contract.Components.Schemas[schemaName]
	typeParameters := ""
	if generic {
		typeParameters = "[T any]"
	}
	fmt.Fprintf(output, "// %s is generated from components.schemas.%s.\ntype %s%s struct {\n", goName, schemaName, goName, typeParameters)
	order := []string{"code", "message", "stacktraces", "version", "execution_optimistic", "finalized", "data", "ethertest_tainted"}
	for _, propertyName := range order {
		property, exists := value.Properties[propertyName]
		if !exists {
			continue
		}
		fieldName := map[string]string{
			"code": "Code", "message": "Message", "stacktraces": "Stacktraces",
			"version": "Version", "execution_optimistic": "ExecutionOptimistic",
			"finalized": "Finalized", "data": "Data", "ethertest_tainted": "EthertestTainted",
		}[propertyName]
		fieldType := goSchemaType(propertyName, property, generic)
		tag := propertyName
		if !slices.Contains(value.Required, propertyName) {
			tag += ",omitempty"
		}
		fmt.Fprintf(output, "\t%s %s `json:\"%s\"`\n", fieldName, fieldType, tag)
	}
	output.WriteString("}\n\n")
}

func goSchemaType(name string, value schema, generic bool) string {
	if name == "data" && generic {
		return "T"
	}
	switch value.Type {
	case "integer":
		return "int"
	case "string":
		return "string"
	case "boolean":
		return "bool"
	case "array":
		if value.Items == nil || value.Items.Type != "string" {
			panic(fmt.Sprintf("unsupported array schema for %s", name))
		}
		return "[]string"
	case "":
		if name == "data" {
			return "any"
		}
	}
	panic(fmt.Sprintf("unsupported schema property %s type %q", name, value.Type))
}

func writeEnumSet(output *bytes.Buffer, name string, values []string) {
	if len(values) == 0 {
		panic(fmt.Sprintf("generated enum %s is empty", name))
	}
	fmt.Fprintf(output, "var %s = map[string]struct{}{\n", name)
	for _, value := range values {
		fmt.Fprintf(output, "\t%q: {},\n", value)
	}
	output.WriteString("}\n\n")
}

func parameterByName(parameters []parameter, name string) parameter {
	for _, value := range parameters {
		if value.Name == name {
			return value
		}
	}
	panic(fmt.Sprintf("request parameter %q is missing", name))
}

func sameStrings(left, right []string) bool {
	return slices.Equal(left, right)
}
