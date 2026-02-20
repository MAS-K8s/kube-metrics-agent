package dto

import "time"

type Config struct {
	Namespace      string        `json:"namespace"`
	Deployment     string        `json:"deployment"`
	PromURL        string        `json:"promurl"`
	RLAgentURL     string        `json:"rlagenturl"`
	Interval       time.Duration `json:"interval"`
	MinReplicas    int32         `json:"minreplicas"`
	MaxReplicas    int32         `json:"maxreplicas"`
	TrainingMode   bool          `json:"trainingmode"`
	MockMode       bool          `json:"mockmode"`
	SafetyEnabled  bool          `json:"safetyenabled"`
	MaxScalePerMin int32         `json:"maxscalepermin"`
	ModelPath      string        `json:"modelpath"`
}
