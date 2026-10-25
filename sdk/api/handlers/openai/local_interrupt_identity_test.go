package openai

import "testing"

func TestLocalInterruptRequiresAnIdentifiedActiveResponse(t *testing.T) {
	local := newResponsesLocalInterrupt()
	local.begin()
	if local.deliver([]byte(`{"type":"response.interrupt","response_id":"unknown"}`)) {
		t.Fatal("unidentified response accepted a local interrupt")
	}
}

func TestLocalInterruptTracksResponseIdentityAndCompletion(t *testing.T) {
	local := newResponsesLocalInterrupt()
	local.begin()
	local.observeResponse([]byte(`{"type":"response.created","response":{"id":"first"}}`))
	if local.deliver([]byte(`{"type":"response.interrupt","response_id":"other"}`)) {
		t.Fatal("wrong response interrupted active turn")
	}
	if !local.deliver([]byte(`{"type":"response.interrupt","response_id":"first"}`)) {
		t.Fatal("matching active interrupt rejected")
	}
	local.observeResponse([]byte(`{"type":"response.completed","response":{"id":"first"}}`))
	if local.deliver([]byte(`{"type":"response.interrupt","response_id":"first"}`)) {
		t.Fatal("terminal response remained interruptible")
	}
	select {
	case <-local.framesChan():
		t.Fatal("terminal turn retained a pending interrupt")
	default:
	}
	local.observeResponse([]byte(`{"type":"response.created","response":{"id":"second"}}`))
	local.observeResponse([]byte(`{"type":"response.completed","response":{"id":"first"}}`))
	if local.deliver([]byte(`{"type":"response.interrupt","response_id":"first"}`)) {
		t.Fatal("old turn interrupted replacement")
	}
	if !local.deliver([]byte(`{"type":"response.interrupt","response_id":"second"}`)) {
		t.Fatal("old completion cleared new turn")
	}
}
