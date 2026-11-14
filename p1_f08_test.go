package main

import "testing"

func TestP1F08BareParentActualModel(t *testing.T) {
 for _, global := range []bool{false,true} {
  a := newDefaultModelsApp(t)
  a.Cfg.DefaultModel = ModelRef{}
  if global { a.Cfg.DefaultModel = ModelRef{Provider:"router",ProviderKey:"k1",Model:"shared"} }
  parent := &SessionMeta{Model:"claude-x"}
  actual, err := a.resolveTarget(parent)
  if err != nil { t.Fatal(err) }
  ctx := &toolContext{app:a,sess:parent}
  p,k,m,err := ctx.dispatchSubModelChoice("")
  if err != nil || p != actual.ProviderID || k != actual.KeyID || m != actual.Model {
   t.Fatalf("global=%v: parent=%s/%s/%s inherited=%s/%s/%s err=%v", global,actual.ProviderID,actual.KeyID,actual.Model,p,k,m,err)
  }
 }
}
