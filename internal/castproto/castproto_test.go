package castproto

import (
	"bytes"
	"testing"
)

func TestFramingAndProtobuf(t *testing.T) {
	payload := `{"type":"LOAD","requestId":42,"media":{"contentId":"https://example.com/stream.m3u8"}}`
	msg := NewStringMessage("sender-0", "web-1", NamespaceMedia, payload)

	var buf bytes.Buffer
	if err := WriteFramedMessage(&buf, msg); err != nil {
		t.Fatalf("WriteFramedMessage failed: %v", err)
	}

	readMsg, err := ReadFramedMessage(&buf)
	if err != nil {
		t.Fatalf("ReadFramedMessage failed: %v", err)
	}

	if readMsg.SourceId != msg.SourceId {
		t.Errorf("SourceId mismatch: got %s, want %s", readMsg.SourceId, msg.SourceId)
	}
	if readMsg.DestinationId != msg.DestinationId {
		t.Errorf("DestinationId mismatch: got %s, want %s", readMsg.DestinationId, msg.DestinationId)
	}
	if readMsg.Namespace != msg.Namespace {
		t.Errorf("Namespace mismatch: got %s, want %s", readMsg.Namespace, msg.Namespace)
	}
	if readMsg.PayloadUtf8 == nil || *readMsg.PayloadUtf8 != payload {
		t.Errorf("PayloadUtf8 mismatch: got %v, want %s", readMsg.PayloadUtf8, payload)
	}
}
