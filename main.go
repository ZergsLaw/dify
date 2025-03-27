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
	"math/rand"
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

const deepSeekSystemPrompt = "<system>\n    <role>You are an AI assistant specialized in analyzing sales dialogues.</role>\n\n    <objective>Your primary task is to analyze the provided dialogue history between a user (potential property buyer) and an AI sales agent. Your goal is to determine if the user has explicitly and unambiguously agreed to a proposed call or meeting with a human agent.</objective>\n\n    <input_format>\n        You will receive the dialogue history enclosed within `<history>` tags. The content inside these tags is a single string. This string contains the conversation history, with each turn formatted as follows:\n        `msg: [message_number], user: [user's message text], ai: [AI agent's message text]`\n        Each turn is separated by a newline character (`\\n`).\n    </input_format>\n\n    <analysis_instructions>\n        1.  **Read Carefully:** Process the *entire* dialogue string provided within the `<history>` tag. Pay attention to the flow of conversation and the roles (`user:` vs `ai:`).\n        2.  **Identify Proposal:** Locate message(s) from the AI agent (`ai:`) that contain a proposal for a synchronous communication channel (e.g., phone call, meeting, video call) to discuss the property purchase further.\n        3.  **Analyze User Response:** Focus specifically on the user's message(s) (`user:`) that immediately follow the AI's proposal.\n        4.  **Evaluate Intent (Strictly):** Your core function is to evaluate the user's intention based *only* on explicit, unambiguous confirmation. Do not infer agreement.\n        5.  **Decision Criteria:** Apply the following rules strictly:\n            <criteria_true>\n                Set the result to `true` **ONLY IF** the user gives clear, direct, and explicit confirmation of their agreement to the specific call/meeting proposed. Examples include (but are not limited to, across any language):\n                *   Direct affirmations: \"Yes\", \"Okay\", \"Agreed\", \"I confirm\", \"Let's schedule it\", \"Sounds good\", \"Yes, let's talk\".\n                *   Explicit confirmation of a proposed time/date or proposing a concrete alternative time/date and confirming availability: \"Yes, 2 PM works\", \"Okay, booked for Tuesday\", \"I can do Friday at 10 AM, let's do that\".\n            </criteria_true>\n            <criteria_false>\n                Set the result to `false` **in ALL other scenarios**. This includes (but is not limited to, across any language):\n                *   Direct refusals: \"No\", \"I can't\", \"Not interested\", \"Not now\".\n                *   Evasive or non-committal replies: \"I'll think about it\", \"Maybe\", \"Perhaps later\", \"Let me check\".\n                *   **Crucially: Any clarifying questions asked by the user *without* also providing explicit confirmation.** Examples: \"When?\", \"Where?\", \"How long?\", \"What will we discuss?\", \"Is morning okay?\". These questions alone signify the user is still evaluating and has *not* agreed.\n                *   Ignoring the proposal or changing the topic.\n                *   Expressing doubts, conditions, or uncertainty: \"Only if...\", \"I might...\", \"I'm not sure yet\".\n                *   Cancelling or expressing hesitation about a previously potentially agreed-upon arrangement within the provided history.\n            </criteria_false>\n        6.  **Context and Final Decision:** Consider the full dialogue. If there are multiple proposals or if the user changes their mind, base your final determination on the user's *last relevant statement* concerning the *most recent proposal* for a call/meeting found in the history.\n        7.  **Default to False:** If, after careful analysis, the user's intention regarding explicit agreement remains ambiguous or unclear for any reason, you MUST default the result to `false`.\n    </analysis_instructions>\n\n    <language_note>\n        The dialogue may occur in **any language**. You must perform the analysis based on the *semantic meaning and intent* of the user's response concerning explicit confirmation. Recognize universal patterns of agreement and refusal, rather than relying solely on keywords from a single language.\n    </language_note>\n\n    <output_specification>\n        **CRITICAL:** Your entire output MUST be a single JSON object and nothing else.\n        *   If the user has explicitly agreed according to the criteria: `{\"client_is_prepared\": true}`\n        *   If the user has NOT explicitly agreed (including ambiguity): `{\"client_is_prepared\": false}`\n\n        **Do not include any introductory text, explanations, summaries, apologies, or any characters outside of the required JSON object.**\n    </output_specification>\n</system>"

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

	var res *dify.ChatMessageResponse
	err := withRetry(ctx, a.log, "dify.ChatMessages", 3, func() error {
		var err error
		res, err = a.dify.API().ChatMessages(ctx, &dify.ChatMessageRequest{
			Inputs: map[string]interface{}{
				"tag": r.Tag,
			},
			Query:          msg,
			ResponseMode:   "blocking",
			ConversationID: r.ConversationId,
			User:           strconv.Itoa(r.LeadId),
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("dify.ChatMessages: %w", err)
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

	var deepSeekRes anthropic.MessagesResponse
	err := withRetry(ctx, a.log, "claude.CreateMessages", 3, func() error {
		var err error
		deepSeekRes, err = a.claude.CreateMessages(ctx, anthropic.MessagesRequest{
			Model: anthropic.ModelClaude3Dot5Sonnet20241022,
			MultiSystem: anthropic.NewMultiSystemMessages(
				deepSeekSystemPrompt,
			),
			Messages: []anthropic.Message{
				anthropic.NewUserTextMessage("<history>" + dialogue + "</history>"),
			},
			MaxTokens: 100,
		})
		return err
	})
	if err != nil {
		return false, fmt.Errorf("claude.CreateMessages: %w", err)
	}

	// { "client_is_prepared": true }

	var jsonRes struct {
		ClientIsPrepared bool `json:"client_is_prepared"`
	}

	if err := json.Unmarshal([]byte(deepSeekRes.Content[0].GetText()), &jsonRes); err != nil {
		return false, fmt.Errorf("json.Unmarshal: %w", err)
	}

	if !jsonRes.ClientIsPrepared {
		return false, nil
	}

	// HTTP request to backend - also add retry
	const u = `https://dev.includecrm.ru/guyfullin/difyai/`

	var resp *http.Response
	err = withRetry(ctx, a.log, "http.Get", 3, func() error {
		var err error
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s?user_id=%s", u, userID), nil)
		if err != nil {
			return err
		}
		resp, err = a.http.Do(req)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("http request: %w", err)
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

// Add this utility function for retries
func withRetry(ctx context.Context, log *slog.Logger, operation string, maxAttempts int, fn func() error) error {
	var err error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}

		if attempt == maxAttempts {
			return fmt.Errorf("%s: all %d attempts failed: %w", operation, maxAttempts, err)
		}

		// Calculate backoff delay with exponential increase and some jitter
		backoff := time.Duration(100*attempt*attempt) * time.Millisecond
		jitter := time.Duration(rand.Intn(100)) * time.Millisecond
		delay := backoff + jitter

		log.Warn("operation failed, retrying",
			slog.String("operation", operation),
			slog.Int("attempt", attempt),
			slog.String("error", err.Error()),
			slog.String("next_retry_in", delay.String()))

		select {
		case <-time.After(delay):
			// Continue to next attempt
		case <-ctx.Done():
			return fmt.Errorf("%s: context canceled during retry: %w", operation, ctx.Err())
		}
	}

	return err
}
