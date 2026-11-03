package sitestats

import "encoding/json"

// Byte counters exceed JavaScript's exact-number range on long-lived accounts.
// Send decimal strings while retaining int64 throughout parsing and SQLite.
func (s Snapshot) MarshalJSON() ([]byte, error) {
	type base Snapshot
	return json.Marshal(struct {
		base
		Upload      string      `json:"upload"`
		Download    string      `json:"download"`
		SeedingSize string      `json:"seedingSize"`
		Seeds       [][2]string `json:"seedingInfo"`
	}{base(s), stringInt(s.Upload), stringInt(s.Download), stringInt(s.SeedingSize), SeedStrings(s.Seeds)})
}
func (s *Snapshot) UnmarshalJSON(raw []byte) error {
	type base Snapshot
	v := struct {
		*base
		Upload      json.Number     `json:"upload"`
		Download    json.Number     `json:"download"`
		SeedingSize json.Number     `json:"seedingSize"`
		Seeds       json.RawMessage `json:"seedingInfo"`
	}{base: (*base)(s)}
	if e := json.Unmarshal(raw, &v); e != nil {
		return e
	}
	var e error
	if s.Upload, e = v.Upload.Int64(); e != nil {
		return e
	}
	if s.Download, e = v.Download.Int64(); e != nil {
		return e
	}
	s.SeedingSize, e = v.SeedingSize.Int64()
	return e
}
func (h History) MarshalJSON() ([]byte, error) {
	type base History
	return json.Marshal(struct {
		base
		Upload      string `json:"upload"`
		Download    string `json:"download"`
		SeedingSize string `json:"seedingSize"`
	}{base(h), stringInt(h.Upload), stringInt(h.Download), stringInt(h.SeedingSize)})
}
func SeedStrings(rows [][2]int64) [][2]string {
	out := make([][2]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, [2]string{stringInt(r[0]), stringInt(r[1])})
	}
	return out
}
