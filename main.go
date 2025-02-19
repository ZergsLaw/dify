package main

// Proxy сервер, который отслеживает каждый прилетевший запрос, сохраняет,

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ZergsLaw/dify-sdk-go"
)

//type request struct {
//	Inputs         map[string]interface{} `json:"inputs"`
//	Query          string                 `json:"query"`
//	ResponseMode   string                 `json:"response_mode"`
//	ConversationId string                 `json:"conversation_id"`
//	User           string                 `json:"user"`
//	Files          json.RawMessage        `json:"files"`
//}

type request struct {
	ChatId         string `json:"chat_id"`
	LeadId         int    `json:"lead_id"`
	TalkId         int    `json:"talk_id"`
	ContactId      int    `json:"contact_id"`
	Message        string `json:"message"`
	ConversationId string `json:"conversation_id"`
	Tag            string `json:"tag"`
}

type AMOMsg struct {
	ChatId         string `json:"chat_id"`
	LeadId         int    `json:"lead_id"`
	TalkId         int    `json:"talk_id"`
	ContactId      int    `json:"contact_id"`
	Answer         string `json:"answer"`
	ConversationId string `json:"conversation_id"`
}

type key struct {
	conversationID string
	userID         int
}

type value struct {
	sync.Mutex
	requests []request
	t        time.Time
}

type cache struct {
	sync.Mutex
	m map[key]*value
}

type api struct {
	c    *cache
	log  *slog.Logger
	http *http.Client
	dify *dify.Client
}

func (a *api) ServeHTTP(writer http.ResponseWriter, r *http.Request) {
	a.handler(writer, r)
}

func (a *api) handler(w http.ResponseWriter, r *http.Request) {

	js, err := io.ReadAll(r.Body)
	if err != nil {
		a.log.Error("error reading request", slog.String("error", err.Error()))
		a.error(w, err)

		return
	}

	a.log.Info("request received", slog.String("method", r.Method), slog.String("url", r.URL.String()), slog.String("body", string(js)))

	convertedJS := strings.ReplaceAll(string(js), "\n", " ")

	var req request
	if err := json.Unmarshal([]byte(convertedJS), &req); err != nil {
		a.log.Error("error decoding request", slog.String("error", err.Error()))
		a.error(w, err)

		return
	}

	a.c.Lock()
	defer a.c.Unlock()

	k := key{conversationID: req.ConversationId, userID: req.LeadId}
	if _, ok := a.c.m[k]; !ok {
		v := &value{
			Mutex:    sync.Mutex{},
			requests: []request{req},
			t:        time.Now(),
		}

		a.c.m[k] = v
	} else {
		v := a.c.m[k]
		v.Lock()
		defer v.Unlock()

		v.requests = append(v.requests, req)
		v.t = time.Now()
	}

	w.WriteHeader(http.StatusNoContent)
}

func (a *api) error(w http.ResponseWriter, err error) {
	errMsg := map[string]string{"error": err.Error()}
	if marshalErr := json.NewEncoder(w).Encode(errMsg); marshalErr != nil {
		a.log.Error("error encoding error message", slog.String("error", marshalErr.Error()))
		return
	}

	w.WriteHeader(http.StatusInternalServerError)
}

func (a *api) process(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.c.Lock()
			for k, v := range a.c.m {
				v.Lock()
				v.Unlock()
				if time.Since(v.t) > time.Second*10 {
					err := a.do(ctx, v)
					if err != nil {
						a.log.Error("error processing request", slog.String("error", err.Error()))

						continue
					}
					delete(a.c.m, k)
				}

			}
			a.c.Unlock()
		}
	}
}

func (a *api) do(ctx context.Context, v *value) error {
	var msg string

	for _, req := range v.requests {
		msg += req.Message + " "
	}

	msg = strings.Replace(msg, "\n", " ", -1)

	r := v.requests[0]

	res, err := a.dify.API().ChatMessages(ctx, &dify.ChatMessageRequest{
		Inputs: map[string]interface{}{
			"tag": r.Tag,
		},
		Query:          msg,
		ResponseMode:   "blocking",
		ConversationID: r.ConversationId,
		User:           strconv.Itoa(r.LeadId),
	})
	if err != nil {
		return fmt.Errorf("a.dify.API().ChatMessages: %w", err)
	}

	a.log.Info("response received", slog.String("response", res.Answer))

	buf, err := json.Marshal(AMOMsg{
		ChatId:         r.ChatId,
		LeadId:         r.LeadId,
		TalkId:         r.TalkId,
		ContactId:      r.ContactId,
		Answer:         res.Answer,
		ConversationId: res.ConversationID,
	})
	if err != nil {
		return fmt.Errorf("json.Marshal: %w", err)
	}

	buffer := bytes.NewBuffer(buf)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://dev.includecrm.ru/guyfullin/difyai/to_amo.php", buffer)
	if err != nil {
		return fmt.Errorf("http.NewRequestWithContext: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("a.http.Do: %w", err)
	}
	defer resp.Body.Close()

	responseMsg, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("io.ReadAll: %w", err)
	}

	a.log.Info("response sent", slog.String("response", string(responseMsg)))

	return nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelInfo,
	}))

	c := &cache{m: make(map[key]*value)}
	d := dify.NewClient("https://api.dify.ai", "app-LOzxzDj52W9npfQp8bLImoKJ")

	a := &api{
		c:    c,
		log:  log,
		http: &http.Client{},
		dify: d,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGABRT, syscall.SIGTERM)
	defer cancel()
	go a.forceShutdown(ctx)

	srv := &http.Server{
		Addr:    "0.0.0.0:8080",
		Handler: a,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil {
			log.Error("srv.ListenAndServe", slog.String("error", err.Error()))
		}
	}()

	go a.process(ctx)

	<-ctx.Done()

	if err := srv.Shutdown(context.Background()); err != nil {
		log.Error("srv.Shutdown", slog.String("error", err.Error()))
	}

	log.Info("shutdown")
}

func (a *api) forceShutdown(ctx context.Context) {
	const shutdownDelay = 15 * time.Second

	<-ctx.Done()
	time.Sleep(shutdownDelay)

	a.log.Error("failed to graceful shutdown")
	os.Exit(2)
}
