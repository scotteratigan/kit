package internal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRestartTask(t *testing.T) {
	dag := DAG[*TaskNode]{
		Nodes: map[string]*TaskNode{
			"build": {
				Name:   "build",
				cancel: func() {},
				mu:     &sync.Mutex{},
			},
		},
	}

	t.Run("unknown task returns 404", func(t *testing.T) {
		control := make(chan any, 1)
		h := newHandler(dag, &sync.Map{}, control)

		req := httptest.NewRequest(http.MethodPost, "/tasks/missing/restart", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusNotFound, rr.Code)
		assert.Empty(t, control)
	})

	t.Run("known task enqueues restart", func(t *testing.T) {
		control := make(chan any, 1)
		h := newHandler(dag, &sync.Map{}, control)

		req := httptest.NewRequest(http.MethodPost, "/tasks/build/restart", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusAccepted, rr.Code)
		assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))

		var body map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Equal(t, true, body["ok"])
		assert.Equal(t, "build", body["task"])

		select {
		case got := <-control:
			assert.Equal(t, "build", got)
		default:
			t.Fatal("expected task name on control channel")
		}
	})

	t.Run("full control channel returns 503", func(t *testing.T) {
		control := make(chan any) // unbuffered, no receiver
		h := newHandler(dag, &sync.Map{}, control)

		req := httptest.NewRequest(http.MethodPost, "/tasks/build/restart", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	})
}
