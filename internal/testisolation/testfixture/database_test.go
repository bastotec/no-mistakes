package testfixture

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabaseRealCommitRollbackAndFreshStorage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	handler, closeDB, err := DatabaseHandler(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	server := httptest.NewServer(handler)
	defer server.Close()
	post := func(body string, want int) string {
		t.Helper()
		response, err := http.Post(server.URL+"/transaction", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		out, _ := io.ReadAll(response.Body)
		if response.StatusCode != want {
			t.Fatalf("%d: %s", response.StatusCode, out)
		}
		return string(out)
	}
	result := post(`[{"sql":"CREATE TABLE item(id INTEGER PRIMARY KEY, value TEXT)"},{"sql":"INSERT INTO item VALUES(?,?)","args":[1,"kept"]},{"sql":"SELECT value FROM item"}]`, 200)
	if !strings.Contains(result, "kept") {
		t.Fatal(result)
	}
	post(`[{"sql":"INSERT INTO item VALUES(2,'rolled back')"},{"sql":"INSERT INTO missing VALUES(1)"}]`, 400)
	result = post(`[{"sql":"SELECT count(*) FROM item"}]`, 200)
	if !strings.Contains(result, `[[1]]`) {
		t.Fatalf("failed request was not rolled back: %s", result)
	}
	fresh, closeFresh, err := DatabaseHandler(filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeFresh()
	recorder := httptest.NewRecorder()
	fresh.ServeHTTP(recorder, httptest.NewRequest("POST", "/transaction", strings.NewReader(`[{"sql":"SELECT * FROM item"}]`)))
	if recorder.Code != 400 {
		t.Fatal("new invocation inherited prior database")
	}
}

func TestDatabaseRejectsUnboundedOrUnexpectedRequests(t *testing.T) {
	handler, closeDB, err := DatabaseHandler(filepath.Join(t.TempDir(), "private.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/transaction", "", 404}, {"POST", "/admin", "[]", 404}, {"POST", "/transaction?url=external", "[]", 404},
		{"POST", "/transaction", "[]", 400}, {"POST", "/transaction", `[{"sql":"SELECT 1","url":"external"}]`, 400},
		{"POST", "/transaction", `[{"sql":"SELECT 1"}] {}`, 400},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, strings.NewReader(test.body)))
		if recorder.Code != test.status {
			t.Fatalf("%+v: %d", test, recorder.Code)
		}
	}
}
