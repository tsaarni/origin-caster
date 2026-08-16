package castproto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protowire"
)

// Standard Cast V2 Namespaces
const (
	NamespaceConnection = "urn:x-cast:com.google.cast.tp.connection"
	NamespaceHeartbeat  = "urn:x-cast:com.google.cast.tp.heartbeat"
	NamespaceReceiver   = "urn:x-cast:com.google.cast.receiver"
	NamespaceMedia      = "urn:x-cast:com.google.cast.media"
	NamespaceDeviceAuth = "urn:x-cast:com.google.cast.tp.deviceauth"
)

// CastMessage represents the protobuf message used in the Cast V2 protocol.
type CastMessage struct {
	ProtocolVersion int32
	SourceId        string
	DestinationId   string
	Namespace       string
	PayloadType     int32 // 0: STRING, 1: BINARY
	PayloadUtf8     *string
	PayloadBinary   []byte
}

// Marshal encodes CastMessage into protobuf wire format.
func (m *CastMessage) Marshal() ([]byte, error) {
	var b []byte
	// Field 1: protocol_version (required in proto2)
	b = protowire.AppendTag(b, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(m.ProtocolVersion))

	// Field 2: source_id (required in proto2)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendString(b, m.SourceId)

	// Field 3: destination_id (required in proto2)
	b = protowire.AppendTag(b, 3, protowire.BytesType)
	b = protowire.AppendString(b, m.DestinationId)

	// Field 4: namespace (required in proto2)
	b = protowire.AppendTag(b, 4, protowire.BytesType)
	b = protowire.AppendString(b, m.Namespace)

	// Field 5: payload_type (required in proto2)
	b = protowire.AppendTag(b, 5, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(m.PayloadType))
	if m.PayloadUtf8 != nil {
		b = protowire.AppendTag(b, 6, protowire.BytesType)
		b = protowire.AppendString(b, *m.PayloadUtf8)
	}
	if len(m.PayloadBinary) > 0 {
		b = protowire.AppendTag(b, 7, protowire.BytesType)
		b = protowire.AppendBytes(b, m.PayloadBinary)
	}
	return b, nil
}

// UnmarshalCastMessage decodes protobuf wire format into CastMessage.
func UnmarshalCastMessage(data []byte) (*CastMessage, error) {
	m := &CastMessage{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		data = data[n:]
		switch num {
		case 1:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			m.ProtocolVersion = int32(v)
			data = data[n:]
		case 2:
			v, n := protowire.ConsumeString(data)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			m.SourceId = v
			data = data[n:]
		case 3:
			v, n := protowire.ConsumeString(data)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			m.DestinationId = v
			data = data[n:]
		case 4:
			v, n := protowire.ConsumeString(data)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			m.Namespace = v
			data = data[n:]
		case 5:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			m.PayloadType = int32(v)
			data = data[n:]
		case 6:
			v, n := protowire.ConsumeString(data)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			m.PayloadUtf8 = &v
			data = data[n:]
		case 7:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			m.PayloadBinary = v
			data = data[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			data = data[n:]
		}
	}
	return m, nil
}

// ReadFramedMessage reads a 4-byte big-endian length prefixed CastMessage from r.
func ReadFramedMessage(r io.Reader) (*CastMessage, error) {
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return nil, err
	}
	if length > 10*1024*1024 { // Sanity check: max 10MB
		return nil, fmt.Errorf("cast message size too large: %d bytes", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("failed to read full cast message (%d bytes): %w", length, err)
	}
	return UnmarshalCastMessage(buf)
}

// WriteFramedMessage writes a 4-byte big-endian length prefixed CastMessage to w.
func WriteFramedMessage(w io.Writer, msg *CastMessage) error {
	data, err := msg.Marshal()
	if err != nil {
		return fmt.Errorf("failed to marshal cast message: %w", err)
	}
	length := uint32(len(data))
	if err := binary.Write(w, binary.BigEndian, length); err != nil {
		return fmt.Errorf("failed to write message length: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("failed to write message payload: %w", err)
	}
	return nil
}

// NewStringMessage is a convenience constructor for UTF-8 JSON CastMessage.
func NewStringMessage(sourceId, destId, namespace, jsonPayload string) *CastMessage {
	return &CastMessage{
		ProtocolVersion: 0,
		SourceId:        sourceId,
		DestinationId:   destId,
		Namespace:       namespace,
		PayloadType:     0,
		PayloadUtf8:     &jsonPayload,
	}
}

// GenericPayload provides a quick way to inspect the message type and requestId.
type GenericPayload struct {
	Type      string `json:"type"`
	RequestId int    `json:"requestId,omitempty"`
}

// HeartbeatPayload represents PING / PONG messages.
type HeartbeatPayload struct {
	Type string `json:"type"`
}

// ConnectionPayload represents CONNECT / CLOSE messages.
type ConnectionPayload struct {
	Type      string                 `json:"type"`
	Origin    map[string]interface{} `json:"origin,omitempty"`
	UserAgent string                 `json:"userAgent,omitempty"`
}

// MediaItem represents media structure inside a LOAD or MEDIA_STATUS message.
type MediaItem struct {
	ContentId        string                 `json:"contentId"`
	StreamType       string                 `json:"streamType,omitempty"`
	ContentType      string                 `json:"contentType,omitempty"`
	Duration         float64                `json:"duration,omitempty"`
	Metadata         map[string]interface{} `json:"metadata,omitempty"`
	CustomData       map[string]interface{} `json:"customData,omitempty"`
	Tracks           []interface{}          `json:"tracks,omitempty"`
	TextTrackStyle   map[string]interface{} `json:"textTrackStyle,omitempty"`
	HlsSegmentFormat string                 `json:"hlsSegmentFormat,omitempty"`
	HlsVideoSegmentFormat string            `json:"hlsVideoSegmentFormat,omitempty"`
}

// LoadMediaPayload represents the urn:x-cast:com.google.cast.media LOAD payload.
type LoadMediaPayload struct {
	Type         string                 `json:"type"`
	RequestId    int                    `json:"requestId"`
	SessionId    string                 `json:"sessionId,omitempty"`
	Media        MediaItem              `json:"media"`
	Autoplay     *bool                  `json:"autoplay,omitempty"`
	CurrentTime  float64                `json:"currentTime,omitempty"`
	PlaybackRate float64                `json:"playbackRate,omitempty"`
	CustomData   map[string]interface{} `json:"customData,omitempty"`
	ActiveTrackIds []int                `json:"activeTrackIds,omitempty"`
}

// ParseGenericPayload parses the message type from JSON string.
func ParseGenericPayload(payload string) (*GenericPayload, error) {
	if payload == "" {
		return nil, errors.New("empty payload")
	}
	var gp GenericPayload
	if err := json.Unmarshal([]byte(payload), &gp); err != nil {
		return nil, err
	}
	return &gp, nil
}
