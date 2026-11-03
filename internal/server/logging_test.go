package server

import (
	"errors"
	"github.com/tsaarni/origin-caster/internal/proxy"
	"net/http"
	"net/http/httptest"
	"testing"
)

type failingController struct{}

func (failingController) CastMedia(proxy.CastRequest) error { return errors.New("offline") }
func (failingController) Play() error                       { return errors.New("offline") }
func (failingController) Pause() error                      { return errors.New("offline") }
func (failingController) Stop() error                       { return errors.New("offline") }
func (failingController) Seek(float64) error                { return errors.New("offline") }
func (failingController) SetVolume(float64, bool) error     { return errors.New("offline") }
func (failingController) GetStatus() proxy.PlaybackState    { return proxy.PlaybackState{} }

func TestControlFailuresReturnErrors(t *testing.T) {
	s := NewServer(proxy.NewServer("http://localhost:8888"))
	s.SetController(failingController{})
	for _, path := range []string{"play", "pause", "stop", "seek?seconds=10", "volume?level=0.5"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/"+path, nil))
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}
