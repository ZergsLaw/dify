package main

// Proxy сервер, который отслеживает каждый прилетевший запрос, сохраняет,

import (
	"bytes"
	"context"
	"encoding/base64"
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

	"github.com/cohesion-org/deepseek-go"
	"github.com/liushuangls/go-anthropic/v2"

	"github.com/ZergsLaw/dify-sdk-go"
)

const deepSeekSystemPrompt = `
<person>
You are an AI Sales Engagement Analyst specializing in real-time conversational intelligence. Your primary mission is to detect authentic buying signals through multimodal pattern recognition, initiating meeting proposals only when 3 strict criteria align.
</person>

<history>
%s
</history>

<core_operating_system>
### Signal Detection Matrix
1. **Intent Quadrant Analysis**  
   - Classify messages across 4 dimensions: Urgency (U), Capability (C), Authority (A), Need (N)  
   - Calculate UCAN score (0-10 per dimension) using:  
     • Lexical density (content words/total words ratio)  
     • Pragmatic markers (e.g., "our team needs", "final decision")  
     • Temporal pressure indicators (deadlines, fiscal year references)

2. **Conversation Thermodynamics**  
   - Map dialogue flow through 3 phases:  
     1. Discovery (Problem Identification) → 2. Solution Matching → 3. Value Quantification  
   - Measure engagement momentum:  
     Δ = (Response depth in characters)/(Time since previous message)^1.5

3. **Proposal Gate Criteria**  
   - Unlock meeting suggestion ONLY when:  
     a) UCAN score ≥ 7 in 3+ dimensions  
     b) Conversation crosses phase 2.5 threshold  
     c) Momentum Δ > 0.85  
     d) No unresolved objections in last 2 exchanges

### Response Activation Protocol
- Initiate booking sequence WHEN:  
  ✔️ User mentions 2+ organizational stakeholders  
  ✔️ Contains implicit/explicit timeline reference  
  ✔️ At least 3 solution-specific terms used  
  ✔️ Message ends with open-ended question

- NEVER interrupt:  
  ❌ Price negotiations in progress  
  ❌ Competitor comparisons  
  ❌ Technical specification requests
</core_operating_system>

<decision_tree>
1. IF UCAN_Score ≥ 28 AND Δ > 0.9 → Immediate calendar integration  
2. IF UCAN_Score 20-27 AND Phase ≥ 2 → Soft proposal ("Would a demo help clarify?")  
3. ELSE → Continue qualification with Socratic questioning  
</decision_tree>`

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
	c        *cache
	log      *slog.Logger
	http     *http.Client
	dify     *dify.Client
	deepseek *deepseek.Client
	claude   *anthropic.Client
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

	if req.Message == "Send me presentation" {
		a.log.Info("ignoring message")
		w.WriteHeader(http.StatusNoContent)

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
				if time.Since(v.t) > time.Second*3 {
					err := a.do(ctx, v)
					if err != nil {
						a.log.Error("error processing request", slog.String("error", err.Error()))
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

	if r.ConversationId != "" {
		messages, err := a.dify.API().Messages(ctx, &dify.MessagesRequest{
			ConversationID: r.ConversationId,
			User:           strconv.Itoa(r.LeadId),
		})
		if err != nil {
			return fmt.Errorf("a.dify.API().Messages: %w", err)
		}

		prepared, err := a.clientIsPrepared(ctx, strconv.Itoa(r.LeadId), messages.Data)
		if err != nil {
			return fmt.Errorf("a.clientIsPrepared: %w", err)
		}

		if prepared {
			return nil
		}
	}

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

func (a *api) clientIsPrepared(ctx context.Context, userID string, history []dify.MessagesDataResponse) (bool, error) {
	if len(history) == 0 {
		return false, nil
	}

	var dialogue string
	for i, response := range history {
		dialogue += fmt.Sprintf("msg: %d, user: %s, ai: %s\n", i+1, response.Query, response.Answer)
	}

	deepSeekRes, err := a.claude.CreateMessages(ctx, anthropic.MessagesRequest{
		Model: anthropic.ModelClaude3Dot5Sonnet20241022,
		MultiSystem: anthropic.NewMultiSystemMessages(
			fmt.Sprintf(deepSeekSystemPrompt, dialogue),
		),
		Messages: []anthropic.Message{
			anthropic.NewUserTextMessage("Detect and return result client is prepared or not by scheme - client is prepared"),
		},
		MaxTokens: 100,
	})
	if err != nil {
		return false, fmt.Errorf("a.deepseek.CreateChatCompletion: %w", err)
	}

	if !strings.Contains(strings.ToLower(deepSeekRes.Content[0].GetText()), "client is prepared") {
		return false, nil
	}

	const u = `https://dev.includecrm.ru/guyfullin/difyai/`

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s?user_id=%s", u, userID), nil)
	if err != nil {
		return false, fmt.Errorf("http.NewRequestWithContext: %w", err)
	}

	resp, err := a.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("a.http.Do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return true, nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelInfo,
	}))

	const token = `c2stYW50LWFwaTAzLXdqNUZnWWt1d2NYc3Vkd19KSHVuNmR0MHlaRmlQQlZnVnFsS0I4UHhBdHJ4RkxoS2d0V1hEdmJOcG1nOHlpZzBhV3QtZTFyMmxaQWZuemY5MWszZC1BLWgtMjlid0FB`

	tokenAnthopoic, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		panic(err)
	}

	c := &cache{m: make(map[key]*value)}
	d := dify.NewClient("https://api.dify.ai", "app-tmQVvWxFW8515uAlzcTnFZkm")

	a := &api{
		c:    c,
		log:  log,
		http: &http.Client{},
		dify: d,
		//deepseek: deepseek.NewClient("sk-f0611df2062a49a29b905c08005c3311"),
		claude: anthropic.NewClient(string(tokenAnthopoic)),
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
