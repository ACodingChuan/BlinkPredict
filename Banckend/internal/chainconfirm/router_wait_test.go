package chainconfirm

import (
	"testing"
	"time"
)

func TestWaitUntilNeededBlocksUntilWakeWithActiveSubscriber(t *testing.T) {
	router := &Router{
		watches: make(map[subscriptionKey]*signatureWatch),
	}
	shard := &routerShard{
		router:          router,
		index:           0,
		wakeCh:          make(chan struct{}, 1),
		pendingRequests: make(map[uint64]subscriptionKey),
		sigByWSSubID:    make(map[uint64]subscriptionKey),
	}

	done := make(chan bool, 1)
	go func() {
		done <- shard.waitUntilNeeded()
	}()

	select {
	case <-done:
		t.Fatal("waitUntilNeeded should block when shard has no subscribers")
	case <-time.After(50 * time.Millisecond):
	}

	key := subscriptionKey{Signature: "sig-1", Commitment: "confirmed"}
	router.mu.Lock()
	router.watches[key] = &signatureWatch{
		Key:         key,
		Signature:   key.Signature,
		Commitment:  key.Commitment,
		ShardIndex:  0,
		Subscribers: map[string]signatureSubscriber{"sub-1": {ID: "sub-1"}},
	}
	router.mu.Unlock()
	shard.signal()

	select {
	case got := <-done:
		if !got {
			t.Fatal("waitUntilNeeded should return true when subscribers become active")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("waitUntilNeeded did not wake after signal")
	}
}

func TestWaitUntilNeededReturnsFalseWhenRouterClosed(t *testing.T) {
	router := &Router{
		watches: make(map[subscriptionKey]*signatureWatch),
	}
	shard := &routerShard{
		router:          router,
		index:           0,
		wakeCh:          make(chan struct{}, 1),
		pendingRequests: make(map[uint64]subscriptionKey),
		sigByWSSubID:    make(map[uint64]subscriptionKey),
	}

	done := make(chan bool, 1)
	go func() {
		done <- shard.waitUntilNeeded()
	}()

	select {
	case <-done:
		t.Fatal("waitUntilNeeded should block before close signal")
	case <-time.After(50 * time.Millisecond):
	}

	router.closed.Store(true)
	shard.signal()

	select {
	case got := <-done:
		if got {
			t.Fatal("waitUntilNeeded should return false when router is closed")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("waitUntilNeeded did not exit after router close")
	}
}
