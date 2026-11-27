package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCleanupAdminAndCSRF(t *testing.T) {
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/cleanup/", nil))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	login := httptest.NewRecorder()
	a.ServeHTTP(login, httptest.NewRequest("GET", "http://127.0.0.1/login?token=synthetic-launch", nil))
	cookie := login.Result().Cookies()[0]
	r := httptest.NewRequest("GET", "http://127.0.0.1/cleanup/", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Cleanup is off by default") {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, path := range []string{"/cleanup/enable", "/cleanup/preview", "/cleanup/apply"} {
		r = httptest.NewRequest("POST", "http://127.0.0.1"+path, strings.NewReader("workspace=fixture"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(cookie)
		w = httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(path, w.Code)
		}
	}
}
