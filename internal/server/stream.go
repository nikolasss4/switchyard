package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/nikolasss4/switchyard/internal/oai"
	"github.com/nikolasss4/switchyard/internal/provider"
	"github.com/nikolasss4/switchyard/internal/sse"
	"github.com/nikolasss4/switchyard/internal/translate"
)

// relayOpenAI writes a canonical stream as OpenAI chunks. Once the first chunk
// is out, an upstream error can only be reported in-band.
func relayOpenAI(w http.ResponseWriter, stream provider.Stream, conv *translate.OpenAIStreamWriter) {
	sw := sse.NewWriter(w)
	for {
		ev, err := stream.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			ae := toAPIError(err)
			b, _ := json.Marshal(oai.ErrorResponse{Error: oai.ErrorBody{Message: ae.Message, Type: ae.Type}})
			_ = sw.Write("", b)
			return
		}
		chunks, done := conv.Event(ev)
		for _, c := range chunks {
			b, _ := json.Marshal(c)
			if sw.Write("", b) != nil {
				return
			}
		}
		if done {
			_ = sw.Write("", []byte("[DONE]"))
			return
		}
	}
}
