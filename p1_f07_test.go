package main

import (
 "net/http/httptest"
 "strings"
 "testing"
)

func TestP1F07SessionBeforeGlobal(t *testing.T) {
 for _, state := range []string{"unset", "disabled", "no-key"} {
  t.Run(state, func(t *testing.T) {
   a := newDefaultModelsApp(t)
   a.Cfg.DefaultModel = ModelRef{}
   if state != "unset" {
    off := false
    p := ProviderCfg{ID:"bad", BaseURL:"https://invalid.example", Models:[]ModelCfg{{ID:"bad"}}}
    if state == "disabled" { p.Enabled = &off }
    a.Cfg.Providers = append(a.Cfg.Providers, p)
    a.Cfg.DefaultModel = ModelRef{Provider:"bad", Model:"bad"}
   }
   sess := &SessionMeta{ID: "aux-"+state, Title:"aux", Mode:"task"}
   sess.Provider, sess.ProviderKey, sess.Model = "router", "k2", "shared"
   if err := a.Store.Create(sess); err != nil { t.Fatal(err) }
   w := httptest.NewRecorder()
   a.handleLLMAsk(w, httptest.NewRequest("POST", "/api/llm/ask", strings.NewReader(`{"session":"`+sess.ID+`","user":"hello"}`)))
   if w.Code != 200 { t.Fatalf("valid session blocked: status=%d body=%s", w.Code, w.Body.String()) }
  })
 }
}
