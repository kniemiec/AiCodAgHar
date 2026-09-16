package config

import "gopkg.in/yaml.v3"

// Condition is a stage's applicability gate — schemas/stages.schema.json's
// "conditions" object.
type Condition struct {
	RiskTier             []string `yaml:"risk_tier,omitempty"`
	FilesTouchedPatterns []string `yaml:"files_touched_patterns,omitempty"`
}

// StageEntry is one entry of .harness/stages.yaml, matching
// schemas/stages.schema.json.
type StageEntry struct {
	ID                string     `yaml:"id"`
	Type              string     `yaml:"type"`            // agent-stage | deterministic-step
	Phase             string     `yaml:"phase,omitempty"` // generate | review | approve | publish, default "review"
	BotAccount        string     `yaml:"bot_account"`
	TaskType          string     `yaml:"task_type"`
	Blocking          bool       `yaml:"blocking"`
	Conditions        *Condition `yaml:"conditions,omitempty"`
	Command           string     `yaml:"command,omitempty"`
	WorkingDir        string     `yaml:"working_dir,omitempty"`
	TimeoutSeconds    int        `yaml:"timeout_seconds,omitempty"`
	ExpectedExitCodes []int      `yaml:"expected_exit_codes,omitempty"`
}

// StageList is the top-level shape of .harness/stages.yaml.
type StageList []StageEntry

// ParseStages parses .harness/stages.yaml content.
func ParseStages(data []byte) (StageList, error) {
	var stages StageList
	if err := yaml.Unmarshal(data, &stages); err != nil {
		return nil, err
	}
	return stages, nil
}
