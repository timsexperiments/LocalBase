package install

import (
	"context"
	"errors"
	"testing"
)

func TestLockModel(t *testing.T) {
	unlock, err := lockModel(context.Background(), t.Name())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lockModel(ctx, t.Name()); !errors.Is(err, context.Canceled) {
		t.Errorf("while held: %v", err)
	}
	if other, err := lockModel(context.Background(), t.Name()+"-other"); err != nil {
		t.Errorf("another model: %v", err)
	} else {
		other()
	}
	unlock()
	again, err := lockModel(context.Background(), t.Name())
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again()
}
