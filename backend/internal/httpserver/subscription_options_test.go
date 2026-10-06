package httpserver

import "testing"

func TestNormalizeNamedOptionsSupportsMapsAndLists(t *testing.T) {
	t.Parallel()
	fromMap := normalizeNamedOptions(map[string]any{"2": "Beta", "1": "Alpha"})
	fromList := normalizeNamedOptions([]any{map[string]any{"id": "7", "name": "Default"}})
	if len(fromMap) != 2 || fromMap[0].Label != "Alpha" || fromMap[0].Value != "1" {
		t.Fatalf("unexpected map options: %#v", fromMap)
	}
	if len(fromList) != 1 || fromList[0].Value != "7" || fromList[0].Label != "Default" {
		t.Fatalf("unexpected list options: %#v", fromList)
	}
}
