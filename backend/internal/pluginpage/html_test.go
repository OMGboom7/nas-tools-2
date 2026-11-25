package pluginpage

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func rendered(t *testing.T, id, markup string) page {
	t.Helper()
	input, err := json.Marshal(map[string]any{"id": id, "title": "临时 https://user:pw@fixture.test/title?k=v", "html": markup})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Convert(bytes.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	var p page
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Title != "临时 fixture.test" {
		t.Fatal(p.Title)
	}
	return p
}
func TestTableColumnsAndActionsStayAligned(t *testing.T) {
	const markup = `<script>secret</script><p>first <b>label</b> https://local.test/path?private=1</p>
<p>first label https://local.test/path?private=1</p><form><p>secret</p></form>
<table><thead><tr><th>影片</th><th>操作</th><th>状态</th></tr></thead><tbody>
<tr id="movie_rank_history_invalid"><td>旧记录</td><td>secret</td><td>完成</td></tr>
<tr id="movie_rank_history_9007199254740993"><td>第二条</td><td><button onclick="secret">secret</button></td><td>待处理</td></tr>
<tr><td>第三条</td></tr></tbody></table>`
	p := rendered(t, "DoubanRank", markup)
	if !p.Omitted || len(p.Sections) != 1 || p.Sections[0] != "first label local.test" || len(p.Tables) != 1 {
		t.Fatal(p)
	}
	tab := p.Tables[0]
	if len(tab.Columns) != 2 || tab.Columns[1] != "状态" || len(tab.Rows) != 3 || tab.Rows[0][1] != "完成" || tab.Rows[2][1] != "" || len(tab.Actions) != 3 || tab.Actions[0] != nil || tab.Actions[2] != nil || tab.Actions[1].ID != "9007199254740993" {
		t.Fatal(tab)
	}
	for _, id := range []string{"OtherExtension", "DoubanSync", "MovieRandom", "MediaLibraryArchive"} {
		p := rendered(t, id, markup)
		if len(p.Tables[0].Actions) != 0 {
			t.Fatal(id, p.Tables)
		}
	}
}
func TestArchiveIdentifiersAndLimits(t *testing.T) {
	const row = `<tr><td><span data-name="归档_20261123010203.md">文件</span></td></tr>`
	const header = `<table><thead><tr><th>文件</th></tr></thead><tbody>`
	raw := header + strings.Repeat(row, 204) + "</tbody></table>"
	p := rendered(t, "MediaLibraryArchive", raw)
	if len(p.Tables) != 1 || len(p.Tables[0].Rows) != 200 || p.Tables[0].Actions[0].ID != "归档_20261123010203.md" {
		t.Fatal("unexpected row limit or archive action")
	}
	p = rendered(t, "MediaLibraryArchive", strings.ReplaceAll(raw, "归档_", "../归档_"))
	if len(p.Tables[0].Actions) != 0 {
		t.Fatal("accepted invalid archive name")
	}
	for _, input := range []string{"{", strings.Repeat("x", maxInput+1)} {
		var out bytes.Buffer
		if err := Convert(strings.NewReader(input), &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid input produced output")
		}
	}
}
