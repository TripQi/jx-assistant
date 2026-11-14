package main

import (
 "strings"
 "testing"
 "os"
 "github.com/dop251/goja"
 "github.com/dop251/goja/parser"
)

func TestP1F09ClientManagerReadOnly(t *testing.T) {
 raw,err := os.ReadFile("组件/todo-scheduler/client.js")
 if err != nil { t.Fatal(err) }
 src := string(raw)
 if _,err := parser.ParseFile(nil,"client.js",src,0); err != nil { t.Fatal(err) }
 vm := goja.New()
 _,err = vm.RunString(`const st={defaults:{manager:{text:'A · High',available:true}},models:[]}; function esc(s){return s;} `)
 if err != nil { t.Fatal(err) }
 for _, pair := range [][2]string{{"modelMetaHTML","onListClick"},{"formEffectiveModel","renderForm"},{"formRequest","formError"},{"formError","recalcOnceDefault"}} {
  start := strings.Index(src,"  function "+pair[0]+"(")
  end := strings.Index(src[start:],"  function "+pair[1]+"(")
  if start < 0 || end < 0 { t.Fatal("missing client function") }
  if _,err := vm.RunString(src[start:start+end]); err != nil { t.Fatal(err) }
 }
 _,err = vm.RunString(`
 const f={executor:'assistant',target:'manager',modelMode:'custom',modelValue:'',kind:'manual',title:'x',description:'x',thinking:'max'};
 const card=modelMetaHTML({target:'manager',modelView:{text:'A · High',available:true}});
 const form=formEffectiveModel(f);
 if (!card.includes('由长期助理当前模型执行（实际：A · High）') || card.includes('当前生效') || card.includes('data-act="edit"')) throw Error(card);
 if (!form.includes('不生效') || form.includes('当前生效')) throw Error(form);
 if (formError(f)) throw Error(formError(f));
 const req=formRequest(f);
 if (req.model!==undefined || req.modelMode!==undefined || req.thinking!==undefined) throw Error(JSON.stringify(req));
 `)
 if err != nil { t.Fatal(err) }
}
func TestP1F09ManagerViewAndValidation(t *testing.T) {
 a := newDefaultModelsApp(t)
 manager := &SessionMeta{ID:"isolated-manager",Mode:"assistant",Provider:"router",ProviderKey:"k2",Model:"claude-x",Thinking:"high"}
 if err := a.Store.Create(manager); err != nil { t.Fatal(err) }
 if err := a.setCurrentAssistant(manager.ID); err != nil { t.Fatal(err) }
 s := &todoScheduler{app:a,busy:map[string]bool{},tombs:map[string]bool{},data:&TodoData{Version:todoDataVersion}}
 a.Cfg.DefaultModel = ModelRef{}
 task := &TaskItem{Executor:execAssistant,Target:targetManager,ModelMode:todoModelCustom,Model:&ModelRef{Provider:"missing",Model:"B"},Thinking:"max"}
 v := s.toView(task,0)
 if v.ModelView == nil || v.ModelView.Model != "claude-x" || v.ModelView.ProviderKey != "k2" || !strings.Contains(v.ModelView.Text,"High") || !strings.Contains(v.ModelView.Source,"长期助理") {
  t.Fatalf("manager view must show actual A/high, not task B/max: %+v",v.ModelView)
 }
 req := todoRequest{Title:"manager",Description:"isolated",Target:targetManager,Schedule:&Schedule{Kind:schedManual},ModelMode:todoModelCustom}
 if _,err := s.create(req); err != nil { t.Fatalf("inactive task model must not block manager: %v",err) }
 got,err := a.Store.Get(manager.ID)
 if err != nil || got.Provider != manager.Provider || got.ProviderKey != manager.ProviderKey || got.Model != manager.Model || got.Thinking != manager.Thinking { t.Fatalf("manager mutated: %+v %v",got,err) }
}
