package message_test

import (
	"errors"
	"reflect"
	"testing"

	"binhttp/internal/message"
)

// FuzzParseRequest asserts two invariants for every payload: decoding never
// panics and fails only with *MalformedError (which the server turns into a
// 400), and whatever decodes survives being encoded and decoded again.
func FuzzParseRequest(f *testing.F) {
	f.Add([]byte{0x01, 0x00, 0x01, '/'})
	f.Add([]byte{0x01, 0x00, 0x02, '/', 'a', 0x01, 0x00, 0x01, 'h', 0x00, 0x00, 0x01, 'x', 0x00, 0x00})
	f.Add([]byte{0x01, 0x00, 0x01, '/', 0x0B, 0x00, 0x00})
	f.Add([]byte{0x01, 0xFF, 0xFF})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, payload []byte) {
		req, err := message.ParseRequest(payload)
		if err != nil {
			var me *message.MalformedError
			if !errors.As(err, &me) {
				t.Fatalf("err = %v, want *MalformedError", err)
			}
			return
		}
		again, err := req.Append(nil)
		if err != nil {
			t.Fatalf("decoded request does not re-encode: %v", err)
		}
		req2, err := message.ParseRequest(again)
		if err != nil {
			t.Fatalf("re-encoded request does not decode: %v", err)
		}
		if !reflect.DeepEqual(normalize(req.Fields), normalize(req2.Fields)) || req.Method != req2.Method || req.Path != req2.Path {
			t.Fatalf("round trip changed the request:\n%+v\n%+v", req, req2)
		}
	})
}

func FuzzParseResponse(f *testing.F) {
	f.Add([]byte{0x00, 0xC8})
	f.Add([]byte{0x01, 0x94, 0x07, 0x00, 0x01, '0'})
	f.Add([]byte{0x00})

	f.Fuzz(func(t *testing.T, payload []byte) {
		resp, err := message.ParseResponse(payload)
		if err != nil {
			var me *message.MalformedError
			if !errors.As(err, &me) {
				t.Fatalf("err = %v, want *MalformedError", err)
			}
			return
		}
		again, err := resp.Append(nil)
		if err != nil {
			t.Fatalf("decoded response does not re-encode: %v", err)
		}
		resp2, err := message.ParseResponse(again)
		if err != nil || resp2.Status != resp.Status || !reflect.DeepEqual(normalize(resp.Fields), normalize(resp2.Fields)) {
			t.Fatalf("round trip changed the response: %+v / %+v / %v", resp, resp2, err)
		}
	})
}

// normalize treats a nil and an empty field list as the same thing.
func normalize(fields []message.Field) []message.Field {
	if len(fields) == 0 {
		return nil
	}
	return fields
}
