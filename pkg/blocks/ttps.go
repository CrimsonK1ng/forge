/*
Copyright © 2023-present, Meta Platforms, Inc. and affiliates
Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:
The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.
THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/

package blocks

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/facebookincubator/ttpforge/pkg/checks"
	"github.com/facebookincubator/ttpforge/pkg/logging"
	"github.com/facebookincubator/ttpforge/pkg/platforms"
	"github.com/spf13/afero"
	"gopkg.in/yaml.v3"
)

// TTP represents the top-level structure for a TTP
// (Tactics, Techniques, and Procedures) object.
//
// **Attributes:**
//
// Environment: A map of environment variables to be set for the TTP.
// Steps: An slice of steps to be executed for the TTP.
// WorkDir: The working directory for the TTP.
type TTP struct {
	PreambleFields `yaml:",inline"`
	Environment    map[string]string `yaml:"env,flow,omitempty"`
	Steps          []Step            `yaml:"steps,omitempty,flow"`
	// Omit WorkDir, but expose for testing.
	WorkDir string `yaml:"-"`
}

// MitreAttack represents mappings to the MITRE ATT&CK framework.
//
// **Attributes:**
//
// Tactics: A string slice containing the MITRE ATT&CK tactic(s) associated with the TTP.
// Techniques: A string slice containing the MITRE ATT&CK technique(s) associated with the TTP.
// SubTechniques: A string slice containing the MITRE ATT&CK sub-technique(s) associated with the TTP.
type MitreAttack struct {
	Tactics       []string `yaml:"tactics,omitempty"`
	Techniques    []string `yaml:"techniques,omitempty"`
	SubTechniques []string `yaml:"subtechniques,omitempty"`
}

// MarshalYAML is a custom marshalling implementation for the TTP structure.
// It encodes a TTP object into a formatted YAML string, handling the
// indentation and structure of the output YAML.
//
// **Returns:**
//
// interface{}: The formatted YAML string representing the TTP object.
// error: An error if the encoding process fails.
func (t *TTP) MarshalYAML() (interface{}, error) {
	marshaled, err := yaml.Marshal(*t)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal TTP to YAML: %v", err)
	}

	// This section is necessary to get the proper formatting.
	// Resource: https://pkg.go.dev/gopkg.in/yaml.v3#section-readme
	m := make(map[interface{}]interface{})

	err = yaml.Unmarshal(marshaled, &m)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal YAML: %v", err)
	}

	b, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal back to YAML: %v", err)
	}

	formattedYAML := reduceIndentation(b, 2)

	return fmt.Sprintf("---\n%s", string(formattedYAML)), nil
}

func reduceIndentation(b []byte, n int) []byte {
	lines := bytes.Split(b, []byte("\n"))

	for i, line := range lines {
		// Replace tabs with spaces for consistent processing
		line = bytes.ReplaceAll(line, []byte("\t"), []byte("    "))

		trimmedLine := bytes.TrimLeft(line, " ")
		indentation := len(line) - len(trimmedLine)
		if indentation >= n {
			lines[i] = bytes.TrimPrefix(line, bytes.Repeat([]byte(" "), n))
		} else {
			lines[i] = trimmedLine
		}
	}

	return bytes.Join(lines, []byte("\n"))
}

// Validate ensures that all components of the TTP are valid
// It checks key fields, then iterates through each step
// and validates them in turn
//
// **Parameters:**
//
// execCtx: The TTPExecutionContext for the current TTP.
//
// **Returns:**
//
// error: An error if any part of the validation fails, otherwise nil.
func (t *TTP) Validate(execCtx TTPExecutionContext) error {
	logging.L().Debugf("Validating TTP %q...", t.Name)

	// Validate preamble fields
	err := t.PreambleFields.Validate()
	if err != nil {
		return err
	}

	// Validate steps
	for _, step := range t.Steps {
		stepCopy := step
		if err := stepCopy.Validate(execCtx); err != nil {
			return err
		}
	}
	logging.L().Debug("...finished validating TTP.")
	return nil
}

func (t *TTP) chdir() (func(), error) {
	// note: t.WorkDir may not be set in tests but should
	// be set when actually using `ttpforge run`
	if t.WorkDir == "" {
		return func() {}, nil
	}
	origDir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(t.WorkDir); err != nil {
		return nil, err
	}
	return func() {
		if err := os.Chdir(origDir); err != nil {
			logging.L().Errorf("could not restore original directory %v: %v", origDir, err)
		}
	}, nil
}

// Execute executes all of the steps in the given TTP,
// then runs cleanup if appropriate
//
// **Parameters:**
//
// execCfg: The TTPExecutionConfig for the current TTP.
//
// **Returns:**
//
// *StepResultsRecord: A StepResultsRecord containing the results of each step.
// error: An error if any of the steps fail to execute.
func (t *TTP) Execute(execCtx *TTPExecutionContext) (*StepResultsRecord, error) {
	logging.L().Infof("RUNNING TTP: %v", t.Name)

	// verify that we actually meet the necessary requirements to execute this TTP
	verificationCtx := checks.VerificationContext{
		Platform: platforms.Spec{
			OS:   runtime.GOOS,
			Arch: runtime.GOARCH,
		},
	}
	if err := t.Requirements.Verify(verificationCtx); err != nil {
		return nil, fmt.Errorf("TTP requirements not met: %w", err)
	}

	stepResults, firstStepToCleanupIdx, runErr := t.RunSteps(execCtx)
	logging.DividerThin()
	if runErr != nil {
		// we need to run cleanup so we don't return here
		logging.L().Errorf("[*] Error executing TTP: %v", runErr)
	} else {
		logging.L().Info("TTP Completed Successfully! ✅")
	}
	if !execCtx.Cfg.NoCleanup {
		if execCtx.Cfg.CleanupDelaySeconds > 0 {
			logging.L().Infof("[*] Sleeping for Requested Cleanup Delay of %v Seconds", execCtx.Cfg.CleanupDelaySeconds)
			time.Sleep(time.Duration(execCtx.Cfg.CleanupDelaySeconds) * time.Second)
		}
		cleanupResults, err := t.startCleanupAtStepIdx(firstStepToCleanupIdx, execCtx)
		if err != nil {
			return nil, err
		}
		// since ByIndex and ByName both contain pointers to
		// the same underlying struct, this will update both
		for cleanupIdx, cleanupResult := range cleanupResults {
			execCtx.StepResults.ByIndex[cleanupIdx].Cleanup = cleanupResult
		}
	}
	// still pass up the run error after our cleanup
	return stepResults, runErr
}

// RunSteps executes all of the steps in the given TTP.
//
// **Parameters:**
//
// execCtx: The current TTPExecutionContext
//
// **Returns:**
//
// *StepResultsRecord: A StepResultsRecord containing the results of each step.
// int: the index of the step where cleanup should start (usually the last successful step)
// error: An error if any of the steps fail to execute.
func (t *TTP) RunSteps(execCtx *TTPExecutionContext) (*StepResultsRecord, int, error) {
	// go to the configuration directory for this TTP
	changeBack, err := t.chdir()
	if err != nil {
		return nil, -1, err
	}
	defer changeBack()

	// actually run all the steps
	stepResults := NewStepResultsRecord()
	execCtx.StepResults = stepResults
	firstStepToCleanupIdx := -1
	for stepIdx, step := range t.Steps {
		stepCopy := step
		logging.DividerThin()
		logging.L().Infof("Executing Step #%d: %q", stepIdx+1, step.Name)

		// core execution - run the step action
		stepResult, err := stepCopy.Execute(*execCtx)

		// this part is tricky - SubTTP steps
		// must be cleaned up even on failure
		// (because substeps may have succeeded)
		// so in those cases, we need to save the result
		// even if nil
		if err != nil {
			if step.ShouldCleanupOnFailure() {
				logging.L().Infof("[+] Cleaning up failed step %s", step.Name)
				logging.L().Infof("[+] Full Cleanup will Run Afterward")
				_, cleanupErr := step.Cleanup(*execCtx)
				if cleanupErr != nil {
					logging.L().Errorf("error cleaning up failed step %v: %v", step.Name, err)
				}
			}
			return nil, firstStepToCleanupIdx, err
		}

		// if the user specified custom success checks, run them now
		verificationCtx := checks.VerificationContext{
			FileSystem: afero.NewOsFs(),
		}
		for checkIdx, check := range step.Checks {
			if err := check.Verify(verificationCtx); err != nil {
				return nil, firstStepToCleanupIdx, fmt.Errorf("success check %d of step %q failed: %w", checkIdx+1, step.Name, err)
			}
			logging.L().Debugf("Success check %d (%q) of step %q PASSED", checkIdx+1, check.Msg, step.Name)
		}

		// step execution successful - record results
		firstStepToCleanupIdx++
		execResult := &ExecutionResult{
			ActResult: *stepResult,
		}
		stepResults.ByName[step.Name] = execResult
		stepResults.ByIndex = append(stepResults.ByIndex, execResult)
	}
	return stepResults, firstStepToCleanupIdx, nil
}

func (t *TTP) startCleanupAtStepIdx(firstStepToCleanupIdx int, execCtx *TTPExecutionContext) ([]*ActResult, error) {
	// go to the configuration directory for this TTP
	changeBack, err := t.chdir()
	if err != nil {
		return nil, err
	}
	defer changeBack()

	logging.DividerThick()
	logging.L().Infof("CLEANING UP TTP: %q", t.Name)
	var cleanupResults []*ActResult
	for cleanupIdx := firstStepToCleanupIdx; cleanupIdx >= 0; cleanupIdx-- {
		stepToCleanup := t.Steps[cleanupIdx]
		logging.DividerThin()
		logging.L().Infof("Cleaning Up Step #%d: %q", cleanupIdx+1, stepToCleanup.Name)
		cleanupResult, err := stepToCleanup.Cleanup(*execCtx)
		// must be careful to put these in step order, not in execution (reverse) order
		cleanupResults = append([]*ActResult{cleanupResult}, cleanupResults...)
		if err != nil {
			logging.L().Errorf("error cleaning up step: %v", err)
			logging.L().Errorf("will continue to try to cleanup other steps")
			continue
		}
	}
	logging.DividerThin()
	logging.L().Info("Finished Cleanup Successfully ✅")
	return cleanupResults, nil
}
