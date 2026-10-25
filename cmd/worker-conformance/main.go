// worker-conformance verifies that a RUNNING provider worker implements the
// mechanical parts of the NATS contract
// (https://docs.opencairn.org/architecture/provider-contract):
//
//  1. presence heartbeat in the cairn_worker_presence KV, fresh
//  2. durable consumers on CAIRN_JOBS for every required job subject —
//     an unconsumed subject accumulates forever in the work-queue stream
//     (the exact failure that silently disabled garmin auto-import)
//  3. discover request/reply answers with a valid shape
//  4. (--active) a synthetic reconcile job round-trips to an
//     account-suffixed reconcile result
//
// Run it against a dev/staging NATS while the worker is up:
//
//	go run ./cmd/worker-conformance --nats nats://localhost:4222 --provider garmin
//
// Exit code 0 = compliant, 1 = violations found.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

type check struct {
	name string
	err  error
}

func main() {
	var (
		natsURL    = flag.String("nats", nats.DefaultURL, "NATS server URL")
		provider   = flag.String("provider", "", "provider name (e.g. garmin, strava)")
		workerName = flag.String("worker-name", "", "worker routing name (default <provider>-fetcher)")
		metrics    = flag.Bool("metrics", false, "worker declares METRIC events (requires import_metrics consumer)")
		gear       = flag.Bool("gear", false, "worker declares the Gear data type (requires import_gear consumer)")
		parseBlob  = flag.Bool("parse-blob", true, "worker archives raw blobs (requires parse_blob consumer)")
		active     = flag.Bool("active", false, "publish a synthetic reconcile job and await its result")
		accountID  = flag.String("account-id", "", "existing external-account id for --active (worker needs its credentials)")
		timeout    = flag.Duration("timeout", 10*time.Second, "per-check timeout")
	)
	flag.Parse()

	if *provider == "" {
		fmt.Fprintln(os.Stderr, "--provider is required")
		os.Exit(2)
	}
	if *workerName == "" {
		*workerName = *provider + "-fetcher"
	}

	nc, err := nats.Connect(*natsURL, nats.Name("worker-conformance"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect %s: %v\n", *natsURL, err)
		os.Exit(2)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jetstream: %v\n", err)
		os.Exit(2)
	}

	required := conformanceRequiredSubjects(*provider, *workerName, *parseBlob, *metrics, *gear)

	var checks []check
	checks = append(checks, check{"presence heartbeat", checkPresence(js, *workerName)})
	for _, subj := range required {
		checks = append(checks, check{"consumer for " + subj, checkConsumer(js, subj)})
	}
	checks = append(checks, check{"discover request/reply", checkDiscover(nc, *provider, *workerName, *timeout)})
	if *active {
		checks = append(checks, check{"reconcile round-trip", checkReconcile(js, *provider, *workerName, *accountID, *timeout)})
	}

	if failed := conformanceReport(checks); failed > 0 {
		fmt.Printf("\n%d violation(s) — see https://docs.opencairn.org/architecture/provider-contract\n", failed)
		os.Exit(1)
	}
	fmt.Println("\nworker is contract-compliant")
}

// conformanceRequiredSubjects lists the job subjects the worker's pool must
// consume (docs/adr/0006: cairn.jobs.<job>.<provider>.<worker_name>).
func conformanceRequiredSubjects(provider, workerName string, parseBlob, metrics, gear bool) []string {
	jobs := []string{"fetch_source", "reconcile"}
	if parseBlob {
		jobs = append(jobs, "parse_blob")
	}
	if metrics {
		jobs = append(jobs, "import_metrics")
	}
	if gear {
		jobs = append(jobs, "import_gear")
	}
	required := make([]string, 0, len(jobs))
	for _, j := range jobs {
		required = append(required, domain.JobSubject(j, provider, workerName))
	}
	return required
}

// conformanceReport prints one PASS/FAIL line per check and returns the failure count.
func conformanceReport(checks []check) int {
	failed := 0
	for _, c := range checks {
		if c.err != nil {
			failed++
			fmt.Printf("FAIL  %-40s %v\n", c.name, c.err)
		} else {
			fmt.Printf("PASS  %s\n", c.name)
		}
	}
	return failed
}

// checkPresence expects a fresh heartbeat under <worker_name>.<instance>.
func checkPresence(js nats.JetStreamContext, workerName string) error {
	kv, err := js.KeyValue("cairn_worker_presence")
	if err != nil {
		return fmt.Errorf("presence KV unavailable: %w", err)
	}
	keys, err := kv.Keys()
	if err != nil {
		return fmt.Errorf("no presence entries at all: %w", err)
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, workerName+".") && k != workerName {
			continue
		}
		entry, err := kv.Get(k)
		if err != nil {
			continue
		}
		var hb struct {
			LastSeen time.Time `json:"last_seen"`
			Provider string    `json:"provider"`
		}
		if err := json.Unmarshal(entry.Value(), &hb); err != nil {
			return fmt.Errorf("presence %s is not valid JSON: %w", k, err)
		}
		if age := time.Since(hb.LastSeen); age > 90*time.Second {
			return fmt.Errorf("presence %s is stale (%s old; heartbeat cadence must be ~20s)", k, age.Round(time.Second))
		}
		return nil
	}
	return fmt.Errorf("no presence key for worker %q — heartbeat loop missing or worker not running", workerName)
}

// checkConsumer requires a durable consumer on CAIRN_JOBS whose filter
// covers the subject. A missing consumer means jobs for that subject pile
// up in the work-queue stream forever.
func checkConsumer(js nats.JetStreamContext, subject string) error {
	for ci := range js.Consumers("CAIRN_JOBS") {
		filters := ci.Config.FilterSubjects
		if ci.Config.FilterSubject != "" {
			filters = append(filters, ci.Config.FilterSubject)
		}
		if len(filters) == 0 {
			return nil // unfiltered consumer covers everything
		}
		for _, f := range filters {
			if subjectMatches(f, subject) {
				return nil
			}
		}
	}
	return fmt.Errorf("no durable consumer filters %s — jobs on it will accumulate unconsumed", subject)
}

// checkDiscover sends an empty-account discover request. Any well-formed
// reply passes — including {"error": ...}: the contract requires a valid
// response shape, not successful listing without credentials.
func checkDiscover(nc *nats.Conn, provider, workerName string, timeout time.Duration) error {
	req, _ := json.Marshal(map[string]any{"account_id": "", "start_page": 1})
	subj := domain.DiscoverSubject(provider, workerName)
	msg, err := nc.Request(subj, req, timeout)
	if err != nil {
		return fmt.Errorf("no reply on %s: %w", subj, err)
	}
	return validateDiscoverReply(msg.Data)
}

// checkReconcile publishes a synthetic reconcile job for accountID and waits
// for the account-suffixed result. Requires a dev stack where the worker can
// resolve that account's credentials.
func checkReconcile(js nats.JetStreamContext, provider, workerName, accountID string, timeout time.Duration) error {
	if accountID == "" {
		return fmt.Errorf("--active requires --account-id of an existing account")
	}
	resultSubj := fmt.Sprintf("cairn.results.reconcile.%s.%s", provider, accountID)
	sub, err := js.SubscribeSync(resultSubj, nats.DeliverNew(), nats.OrderedConsumer())
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", resultSubj, err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	jobID := fmt.Sprintf("conformance-%d", time.Now().UnixNano())
	body, _ := json.Marshal(map[string]any{
		"job_id": jobID, "account_id": accountID, "provider": provider,
		"max_enqueue": 1,
	})
	if _, err := js.Publish(domain.JobSubject("reconcile", provider, workerName), body, nats.MsgId("conformance:"+jobID)); err != nil {
		return fmt.Errorf("publish reconcile job: %w", err)
	}

	// Workers MAY coalesce reconciles (~10 min); a coalesced ack without a
	// result is only detectable via a longer wait, so allow generous time.
	msg, err := sub.NextMsg(timeout)
	if err != nil {
		return fmt.Errorf("no reconcile result on %s within %s (missing handler, or coalesced — retry after the coalescing window)", resultSubj, timeout)
	}
	var jr map[string]any
	if err := json.Unmarshal(msg.Data, &jr); err != nil {
		return fmt.Errorf("reconcile result is not protojson: %w", err)
	}
	if _, ok := jr["workerName"]; !ok {
		return fmt.Errorf("reconcile result missing worker stamp")
	}
	return nil
}

// subjectMatches reports whether a consumer filter covers subject,
// supporting the * and > wildcards.
func subjectMatches(filter, subject string) bool {
	if filter == subject {
		return true
	}
	ft, st := strings.Split(filter, "."), strings.Split(subject, ".")
	for i, f := range ft {
		if f == ">" {
			return true
		}
		if i >= len(st) {
			return false
		}
		if f != "*" && f != st[i] {
			return false
		}
	}
	return len(ft) == len(st)
}

// validateDiscoverReply checks the reply shape from a discover responder.
func validateDiscoverReply(data []byte) error {
	var resp struct {
		Items       []map[string]any `json:"items"`
		NextPage    *int             `json:"next_page"`
		Complete    *bool            `json:"complete"`
		RateLimited *bool            `json:"rate_limited"`
		Error       *string          `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("reply is not valid JSON: %w", err)
	}
	if resp.Error == nil && resp.Items == nil && resp.Complete == nil && resp.NextPage == nil {
		return fmt.Errorf("reply carries none of items/next_page/complete/error: %s", truncate(data, 200))
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
