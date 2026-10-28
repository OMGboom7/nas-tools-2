package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/speedlimit"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

type limiterRateSession interface {
	Read(context.Context) (speedlimit.Rates, error)
	Write(context.Context, string, speedlimit.Rates) error
}

func (api *speedLimiterAPI) openRates(ctx context.Context, target speedlimit.Target) (limiterRateSession, speedlimit.Rates, error) {
	raw, err := api.queue.TargetConfig(ctx, target)
	if err != nil {
		return nil, speedlimit.Rates{}, err
	}
	var values map[string]any
	if json.Unmarshal([]byte(raw), &values) != nil {
		return nil, speedlimit.Rates{}, speedlimit.ErrState
	}
	transport := api.transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if t, ok := transport.(*http.Transport); ok {
		clone := t.Clone()
		clone.DisableKeepAlives = true
		clone.Protocols = new(http.Protocols)
		clone.Protocols.SetHTTP1(true)
		transport = clone
	}
	switch target.Kind {
	case "qbittorrent":
		client, err := qbittorrent.New(text(values["host"]), text(values["port"]), text(values["username"]), text(values["password"]), transport)
		if err != nil {
			return nil, speedlimit.Rates{}, err
		}
		return client.OpenGlobalRates(ctx)
	case "transmission":
		client, err := transmission.New(text(values["host"]), text(values["port"]), text(values["username"]), text(values["password"]), transport)
		if err != nil {
			return nil, speedlimit.Rates{}, err
		}
		return client.OpenGlobalRates(ctx)
	}
	return nil, speedlimit.Rates{}, speedlimit.ErrState
}
func (api *speedLimiterAPI) tick(ctx context.Context) error {
	if api == nil || api.organizer == nil || !api.organizer.pureGo || api.queue == nil {
		return nil
	}
	guard, err := api.organizer.jobs.AcquireJob(ctx, speedLimiterGuard)
	if err != nil {
		return err
	}
	defer guard.Close()
	c, spec, active, err := api.queue.Scope(ctx)
	if err != nil {
		return err
	}
	if active && api.validate(c) == nil {
		if err := api.queue.Timer(ctx, c, spec, time.Now()); err != nil {
			return err
		}
	}
	triggers, err := api.queue.Triggers(ctx, true)
	if err != nil {
		return err
	}
	for _, trigger := range triggers {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, spec, active, err = api.queue.Scope(ctx)
		if err != nil || !active || c.Epoch != trigger.Epoch || api.validate(c) != nil {
			if e := api.queue.BlockTrigger(ctx, trigger, "original playback limiter configuration or authority changed"); e != nil {
				return e
			}
			continue
		}
		client, err := api.connectMedia(ctx, c)
		if err != nil {
			if e := api.queue.BlockTrigger(ctx, trigger, "original media server connection not confirmed"); e != nil {
				return e
			}
			continue
		}
		identity, err := client.RefreshIdentity(ctx)
		if err != nil || identity != c.MediaID {
			if e := api.queue.BlockTrigger(ctx, trigger, "original media server identity not confirmed"); e != nil {
				return e
			}
			continue
		}
		observation, err := client.ObservePlaying(ctx, spec)
		if err != nil {
			if e := api.queue.BlockTrigger(ctx, trigger, "complete live sessions not confirmed; idle limits were not applied"); e != nil {
				return e
			}
			continue
		}
		before := []speedlimit.Baseline{}
		sessions := map[string]limiterRateSession{}
		valid := true
		for _, baseline := range c.Baselines {
			target := baseline.Target
			if pending, e := api.queue.Unresolved(ctx, target.ID); e != nil || pending {
				valid = false
				break
			}
			session, rates, e := api.openRates(ctx, target)
			if e != nil {
				valid = false
				break
			}
			owned, e := api.queue.Owner(ctx, c, target)
			if e != nil || owned != rates {
				valid = false
				break
			}
			before = append(before, speedlimit.Baseline{Target: target, Rates: rates})
			sessions[target.ID] = session
		}
		if !valid {
			if e := api.queue.BlockTrigger(ctx, trigger, "original downloader rates unavailable, changed externally or awaiting an earlier receipt"); e != nil {
				return e
			}
			continue
		}
		job, err := api.queue.CreateJob(ctx, c, spec, trigger, observation, before, func() error { return api.validate(c) })
		if err != nil {
			// Planning happens before any rate mutation. A malformed/incomplete
			// observation is an explicit blocked trigger, never an idle proof.
			if e := api.queue.BlockTrigger(ctx, trigger, "exact original playback/rate plan could not be committed"); e != nil {
				return err
			}
			continue
		}
		failed := map[string]bool{}
		for index, op := range job.Definition.Operations {
			if err := ctx.Err(); err != nil {
				return err
			}
			if failed[op.Target.ID] {
				if err := api.queue.BlockOperation(ctx, job, index, "prior setting for this original downloader is not confirmed"); err != nil {
					return err
				}
				continue
			}
			session := sessions[op.Target.ID]
			current, err := session.Read(ctx)
			if err != nil || current != op.Before {
				failed[op.Target.ID] = true
				if err := api.queue.BlockOperation(ctx, job, index, "complete original rate snapshot changed before dispatch"); err != nil {
					return err
				}
				continue
			}
			if err := api.queue.BeginOperation(ctx, job, index, func() error { return api.validate(c) }); err != nil {
				failed[op.Target.ID] = true
				if errors.Is(err, speedlimit.ErrState) {
					continue
				}
				return err
			}
			err = session.Write(ctx, op.Dimension, op.After)
			confirmed := false
			if err == nil {
				observed, e := session.Read(ctx)
				confirmed = e == nil && observed == op.After
			}
			persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err = api.queue.FinishOperation(persist, job, index, confirmed)
			cancel()
			if err != nil {
				return err
			}
			if !confirmed {
				failed[op.Target.ID] = true
			}
		}
		if err := api.queue.Finalize(ctx, job); err != nil {
			return err
		}
	}
	return nil
}
func startSpeedLimiterWorker(ctx context.Context, api *speedLimiterAPI) func() {
	if api == nil || api.organizer == nil || !api.organizer.pureGo {
		return func() {}
	}
	done := make(chan struct{})
	api.workers.Add(1)
	go func() {
		defer close(done)
		defer api.workers.Add(-1)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run, cancel := context.WithTimeout(ctx, 90*time.Second)
				err := api.tick(run)
				cancel()
				if err != nil && !errors.Is(err, organization.ErrBusy) && !errors.Is(err, context.Canceled) {
					slog.Warn("playback limiter interrupted; original rate receipts retained")
				}
			}
		}
	}()
	return func() { <-done }
}
