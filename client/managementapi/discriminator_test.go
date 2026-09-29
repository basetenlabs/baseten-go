package managementapi

import (
	"encoding/json"
	"testing"
)

func TestOneTimeScheduleUnionSetsDiscriminator(t *testing.T) {
	var schedule EnvironmentAutoscalingSchedules_Schedules_Item
	if err := schedule.FromOneTimeAutoscalingSchedule(OneTimeAutoscalingSchedule{}); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(schedule)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["cadence"] != "ONE_TIME" {
		t.Fatalf("missing discriminator: %s", encoded)
	}
}

func TestOneTimeScheduleUpsertUnionSetsDiscriminator(t *testing.T) {
	var schedule UpdateAutoscalingScheduleSettings_Schedules_Item
	if err := schedule.FromOneTimeAutoscalingScheduleUpsert(OneTimeAutoscalingScheduleUpsert{}); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(schedule)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["cadence"] != "ONE_TIME" {
		t.Fatalf("missing discriminator: %s", encoded)
	}
}
