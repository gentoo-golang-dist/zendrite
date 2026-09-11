package internal

import (
	"testing"
	"time"

	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/process"
)

func TestIdlePartialStateWorkerStops(t *testing.T) {
	p := process.NewProcessContextWithParent(t.Context())
	w := &PartialStateWorker{process: p, workerCh: make(chan types.RoomNID)}
	p.Go(func() { w.worker(0) })
	p.ShutdownZendrite()
	done := make(chan struct{})
	go func() { p.WaitForComponentsToFinish(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle partial-state worker ignored cancellation")
	}
}
