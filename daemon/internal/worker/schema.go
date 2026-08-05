package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

var (
	schemaOnce sync.Once
	compiled   *jsonschema.Schema
	schemaErr  error
)

func planValidator() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		if err := c.AddResource("plan.schema.json", bytes.NewReader([]byte(planSchema))); err != nil {
			schemaErr = err
			return
		}
		compiled, schemaErr = c.Compile("plan.schema.json")
	})
	return compiled, schemaErr
}

// validatePlanSchema re-validates the plan against schemas/plan.schema.json
// (V1). The worker never trusts a single validator (the --json-schema flag
// already constrained the model's output).
func validatePlanSchema(plan Plan) error {
	sch, err := planValidator()
	if err != nil {
		return fmt.Errorf("compile plan schema: %w", err)
	}
	buf, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	var doc any
	if err := json.Unmarshal(buf, &doc); err != nil {
		return err
	}
	if err := sch.Validate(doc); err != nil {
		return err
	}
	return nil
}
