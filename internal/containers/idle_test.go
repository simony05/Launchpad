package containers

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestIdlePreservesContainer(t *testing.T) {
	m := NewDockerManager(time.Second)
	var calls [][]string
	m.run = func(_ context.Context, args ...string) (string, error) { calls = append(calls, args); return "", nil }
	if err := m.Suspend(context.Background(), testContainerID); err != nil {
		t.Fatal(err)
	}
	if err := m.Resume(context.Background(), testContainerID); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"stop", "--time", "10", testContainerID}, {"start", testContainerID}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("%v", calls)
	}
	if err := m.Resume(context.Background(), "--bad"); err == nil {
		t.Fatal("invalid ID accepted")
	}
}
