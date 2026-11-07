package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// Two unloads of one model: the one that gives up must not end the other's drain, or a new
// request gets the model the other is waiting to stop.
func TestAnUnloadGivingUpLeavesTheOtherDraining(t *testing.T) {
	_, llm, _ := newTestGateway(t, host{})
	ctx := t.Context()
	held, err := llm.Acquire(ctx, ChatModel)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		freed int64
		err   error
	}
	patient := make(chan result, 1)
	go func() {
		freed, err := llm.Unload(ctx, ChatModel, 20*time.Second)
		patient <- result{freed, err}
	}()
	waitDraining := time.Now().Add(5 * time.Second)
	for {
		short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		l, err := llm.Acquire(short, ChatModel)
		cancel()
		if errors.Is(err, runtime.ErrUnloading) {
			break
		}
		if l != nil {
			l.Release()
		}
		if time.Now().After(waitDraining) {
			t.Fatalf("the drain never began: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var busy *runtime.BusyError
	if _, err := llm.Unload(ctx, ChatModel, 50*time.Millisecond); !errors.As(err, &busy) {
		t.Fatalf("the impatient unload: %v", err)
	}
	short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	l, err := llm.Acquire(short, ChatModel)
	if l != nil {
		l.Release()
	}
	if !errors.Is(err, runtime.ErrUnloading) {
		t.Errorf("a new request while the other unload still drains: %v, want ErrUnloading", err)
	}
	held.Release()
	select {
	case r := <-patient:
		if r.err != nil || r.freed == 0 {
			t.Errorf("the patient unload: %d %v", r.freed, r.err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("the patient unload never finished")
	}
	if id, _, _ := llm.Running(); id != "" {
		t.Errorf("%q is still loaded", id)
	}
}

// The same on a media service.
func TestAServiceUnloadGivingUpLeavesTheOtherDraining(t *testing.T) {
	const whisperModel = "whisper-base-q8_0" // what newTranscriptionGateway installs
	svc := runServer(newTranscriptionGateway(t)).STT
	ctx := t.Context()
	held, err := svc.Acquire(ctx, whisperModel)
	if err != nil {
		t.Fatal(err)
	}
	patient := make(chan error, 1)
	go func() {
		_, err := svc.Unload(ctx, whisperModel, 20*time.Second)
		patient <- err
	}()
	refused := func() bool {
		short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		l, err := svc.Acquire(short, whisperModel)
		if l != nil {
			l.Release()
		}
		return errors.Is(err, runtime.ErrUnloading)
	}
	for deadline := time.Now().Add(5 * time.Second); !refused(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the drain never began")
		}
	}
	var busy *runtime.BusyError
	if _, err := svc.Unload(ctx, whisperModel, 50*time.Millisecond); !errors.As(err, &busy) {
		t.Fatalf("the impatient unload: %v", err)
	}
	if !refused() {
		t.Error("a new request while the other unload still drains was not refused")
	}
	held.Release()
	select {
	case err := <-patient:
		if err != nil {
			t.Errorf("the patient unload: %v", err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("the patient unload never finished")
	}
}
