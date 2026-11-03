package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

func (s *Store) PrepareApply(ctx context.Context, id string, plan ApplyPlan, result Result) error {
	if plan.FileBeforeHash == "" || len(plan.FileBefore) > 1<<20 || sum([]byte(plan.FileBefore)) != plan.FileBeforeHash || len(plan.FileAfter) > 1<<20 || plan.Hosts.AfterIP != result.BestIP {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	j, err := readJob(ctx, tx, id)
	if err != nil || j.State != "running" || j.Plan != nil {
		return ErrState
	}
	c, err := control(ctx, tx)
	if err != nil || c.Epoch != j.Epoch || c.Source != j.Source {
		return ErrState
	}
	if err = current(ctx, tx, c); err != nil {
		return err
	}
	settings, err := DecodeSettings(j.Source.Config)
	if err != nil {
		return err
	}
	expected, err := PlanHosts(j.Source.Hosts, settings, result.BestIP)
	if err != nil || expected != plan.Hosts {
		return ErrState
	}
	plan.HostsRaw = expected.Raw
	plan.HostsInput = expected.Input
	cfIP, _ := json.Marshal(result.BestIP)
	version, _ := json.Marshal(result.Version)
	plan.Config, err = Patch(j.Source.Config, map[string]json.RawMessage{"cf_ip": cfIP, "version": version, "ipv4": json.RawMessage(fmt.Sprint(settings.IPv4)), "ipv6": json.RawMessage(fmt.Sprint(settings.IPv6))})
	if err != nil {
		return err
	}
	j.Plan = &plan
	j.Result = &result
	j.State = "applying"
	j.Operations = append(j.Operations, Operation{Kind: "hosts", Digest: sum([]byte(plan.FileAfter))})
	if err = saveJob(ctx, tx, j); err != nil {
		return err
	}
	return tx.Commit()
}

// The caller has read the original Hosts path and proved its exact after-image.
// Completion never writes the file; it only reconciles the original DB intent.
func (s *Store) Finalize(ctx context.Context, id, fileHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	j, err := readJob(ctx, tx, id)
	if err != nil {
		return err
	}
	if j.State == "complete" {
		return nil
	}
	if j.State != "applying" && j.State != "unknown" || j.Plan == nil || j.Result == nil || fileHash != sum([]byte(j.Plan.FileAfter)) {
		return ErrState
	}
	actual, err := source(ctx, tx)
	if err != nil || actual != j.Source {
		return ErrState
	}
	c, err := control(ctx, tx)
	if err != nil || c.Epoch != j.Epoch || c.Source != j.Source {
		return ErrState
	}
	if err = writeSetting(ctx, tx, ConfigKey, j.Plan.Config); err != nil {
		return err
	}
	if err = writeSetting(ctx, tx, "plugin.CustomHosts", j.Plan.HostsRaw); err != nil {
		return err
	}
	next := actual
	next.Config = j.Plan.Config
	next.Hosts = j.Plan.HostsRaw
	verified, err := source(ctx, tx)
	if err != nil || verified != next {
		return ErrState
	}
	c.Source = next
	if err = saveControl(ctx, tx, c); err != nil {
		return err
	}
	settings, err := DecodeSettings(j.Source.Config)
	if err != nil {
		return err
	}
	if settings.Notify && j.Plan.Hosts.BeforeIP != j.Plan.Hosts.AfterIP {
		body := fmt.Sprintf("原ip：%s\n新ip：%s", j.Plan.Hosts.BeforeIP, j.Plan.Hosts.AfterIP)
		if err = bus.EmitData(ctx, tx, "plugin_message", j.ID, j.ID, "【Cloudflare优选任务完成】", body, map[string]string{"plugin": Plugin}); err != nil {
			return err
		}
	}
	j.State = "complete"
	j.Reason = "本次测速和原 Hosts／配置更新已核实"
	if err = saveJob(ctx, tx, j); err != nil {
		return err
	}
	return tx.Commit()
}

// Configuration changes revoke future work in the same transaction. They do
// not download/install a program or execute a legacy onlyonce switch.
func (s *Store) Configure(ctx context.Context, expected Source, nextRaw string, installation *bool) error {
	if _, err := DecodeSettings(nextRaw); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actual, err := source(ctx, tx)
	if err != nil || actual != expected {
		return ErrState
	}
	if nextRaw != "" {
		if err = writeSetting(ctx, tx, ConfigKey, nextRaw); err != nil {
			return err
		}
	}
	if installation != nil {
		ids := []string{}
		if actual.Installed != "" && json.Unmarshal([]byte(actual.Installed), &ids) != nil {
			return ErrState
		}
		next := []string{}
		exists := false
		for _, id := range ids {
			if id == Plugin {
				exists = true
				if !*installation {
					continue
				}
			}
			next = append(next, id)
		}
		if *installation && !exists {
			next = append(next, Plugin)
		}
		if err = writeSetting(ctx, tx, "UserInstalledPlugins", encode(next)); err != nil {
			return err
		}
	}
	c, err := control(ctx, tx)
	if err != nil {
		return err
	}
	if c.Epoch != "" {
		c.Enabled = false
		c.NextOnce = ""
		c.NextRun = ""
		if err = saveControl(ctx, tx, c); err != nil {
			return err
		}
	}
	return tx.Commit()
}
