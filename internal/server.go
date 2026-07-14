package internal

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML string

func StartServer(ctx context.Context, port int, wg *sync.WaitGroup, dag DAG[*TaskNode], statusEvents <-chan *TaskNode, control chan<- any) {

	streams := &sync.Map{}

	go func() {
		for event := range statusEvents {
			streams.Range(func(key, value any) bool {
				// non-blocking: a slow client must not stall the broadcast
				select {
				case value.(chan *TaskNode) <- event:
				default:
				}
				return true
			})
		}
	}()

	server := &http.Server{
		Addr:    fmt.Sprintf("localhost:%d", port),
		Handler: newHandler(dag, streams, control),
		BaseContext: func(listener net.Listener) context.Context {
			return ctx
		},
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		if err := server.Shutdown(ctx); err != nil {
			log.Println(err)
		}
	}()

	log.Printf("UI available on http://%s", server.Addr)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		panic(err)
	}
}

func newHandler(dag DAG[*TaskNode], streams *sync.Map, control chan<- any) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// if internal/index.html exists, serve that
		_, err := os.Stat("internal/index.html")
		if err == nil {
			http.ServeFile(w, r, "internal/index.html")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, err = w.Write([]byte(indexHTML))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/dag", func(w http.ResponseWriter, r *http.Request) {
		// return the dag
		marshal, err := json.Marshal(dag)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write(marshal)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {

		id := rand.Int()

		// create a stream for this connection
		stream := make(chan *TaskNode, 100)

		// load the stream with the current state
		for _, node := range dag.Nodes {
			stream <- node
		}
		streams.Store(id, stream)
		defer func() {
			streams.Delete(id)
		}()

		// return an event stream
		w.Header().Set("Content-Type", "text/event-stream")
		for {
			select {
			case <-r.Context().Done():
				return
			case event := <-stream:
				marshal, err := json.Marshal(event)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				_, err = fmt.Fprintf(w, "data: %s\n\n", marshal)
				if err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
		}
	})
	mux.HandleFunc("/logs/{task}", func(w http.ResponseWriter, r *http.Request) {
		task := r.PathValue("task")
		node, ok := dag.Nodes[task]
		if !ok {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}
		file, err := os.Open(node.logFile)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer file.Close()

		w.Header().Set("Content-Type", "text/event-stream")

		// Batch lines into a single SSE event (the browser joins the "data:"
		// lines with "\n") so replaying a large log file doesn't produce one
		// event, and one flush, per line.
		const maxBatchLines = 1000
		var batch bytes.Buffer
		flushBatch := func() error {
			if batch.Len() == 0 {
				return nil
			}
			batch.WriteString("\n")
			if _, err := w.Write(batch.Bytes()); err != nil {
				return err
			}
			w.(http.Flusher).Flush()
			batch.Reset()
			return nil
		}

		for {
			scanner := bufio.NewScanner(file)
			lines := 0
			for scanner.Scan() {
				fmt.Fprintf(&batch, "data: %s\n", scanner.Text())
				lines++
				if lines >= maxBatchLines {
					if err := flushBatch(); err != nil {
						return
					}
					lines = 0
				}
			}
			if err := flushBatch(); err != nil {
				return
			}

			if err := scanner.Err(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			// stop tailing when the client disconnects
			select {
			case <-r.Context().Done():
				return
			case <-time.After(1 * time.Second):
			}

			// Reset the scanner to continue reading new lines
			_, err := file.Seek(0, io.SeekCurrent)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	})
	mux.HandleFunc("POST /tasks/{task}/restart", func(w http.ResponseWriter, r *http.Request) {
		task := r.PathValue("task")
		if _, ok := dag.Nodes[task]; !ok {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}
		select {
		case control <- task:
		default:
			http.Error(w, "restart queue full", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"task": task,
		})
	})
	return mux
}
