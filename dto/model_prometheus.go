package dto

import (
	"sync"
	"time"
)

// type PrometheusResponse struct {
// 	Status string `json:"status"`
// 	Data   struct {
// 		Result []struct {
// 			Value []interface{} `json:"value"`
// 		} `json:"result"`
// 	} `json:"data"`
// }
type PrometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			// Value is [unixTimestamp, valueString].
			Value []interface{} `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

type ScalingEvent struct {
	Timestamp    time.Time `json:"timestamp"`
	FromReplicas int32     `json:"fromreplicas"`
	ToReplicas   int32     `json:"toreplicas"`
	Action       string    `json:"action"`
	Reason       string    `json:"reason"`
}

type SafetyController struct {
	ScalingEvents []ScalingEvent `json:"scalingevents"`
	MaxPerMinute  int32          `json:"maxperminute"`
	mu            sync.Mutex     `json:"-"` // excluded from JSON
}