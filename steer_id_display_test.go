package main

import "testing"

func TestSteerIDDisplay(t *testing.T) {
	t.Run("repeat-and-exact-landing", func(t *testing.T) {
		interjectJS(t, `
applyPending(mainView,{steer:[{id:'a',content:'same'},{id:'b',content:'same'}]});
handleEvent({type:'message',sessionId:'main',seq:10,pendingId:'a',message:{role:'user',content:'same'}},mainView);
assert(state.pendingSteer.length===1 && state.pendingSteer[0].id==='b','only landed ID may disappear');
assert(state.msgs[0].pendingId==='a','event link retained for delayed snapshots');
applyPending(mainView,{steer:[{id:'a',content:'same'},{id:'b',content:'same'},{id:'c',content:'same'}]});
assert(state.pendingSteer.map(p=>p.id).join(',')==='b,c','stale snapshot cannot resurrect landed ID or hide duplicates');
handleEvent({type:'message',sessionId:'main',seq:11,pendingId:'b',message:{role:'user',content:'same'}},mainView);
assert(state.pendingSteer.length===1 && state.pendingSteer[0].id==='c','second landing clears second ID only');
`)
	})
	t.Run("legacy-and-old-history", func(t *testing.T) {
		interjectJS(t, `
state.msgs=[{role:'user',seq:1,content:'same'}];
applyPending(mainView,{steer:[{id:'new',content:'same'}]});
handleEvent({type:'message',seq:2,message:{role:'user',content:'same'}},mainView);
assert(state.pendingSteer.length===1,'unidentified legacy event must not guess by body');
applyPending(mainView,{steer:[]});
assert(state.pendingSteer.length===0,'legacy pending snapshot still clears');
`)
	})
	t.Run("pane-isolation-and-attached", func(t *testing.T) {
		interjectJS(t, `
applyPending(mainView,{steer:[{id:'shared',content:'same'}]});
applyPending(subView,{steer:[{id:'shared',content:'same'},{id:'next',content:'same'}]});
subState.attached=true;
handleEvent({type:'message',sessionId:'main',pendingId:'shared',seq:3,message:{role:'user',content:'same'}},subView);
assert(subState.pendingSteer.length===2,'wrong session cannot clear pane');
handleEvent({type:'message',sessionId:'sub',pendingId:'shared',seq:3,message:{role:'user',content:'same'}},subView);
assert(subState.pendingSteer.length===1 && state.pendingSteer.length===1,'attached pane clears only its own ID');
applyPending(subView,{steer:[{id:'shared'},{id:'next'}]});
assert(subState.pendingSteer.length===1,'attached reload delay must not resurrect ID');
`)
	})
	t.Run("history-projection-and-session-switch", func(t *testing.T) {
		interjectJS(t, `
state.msgs=[{role:'user',seq:5,pendingId:'old',content:'same'}];
applyPending(mainView,{steer:[{id:'old',content:'same'},{id:'new',content:'same'}]});
assert(state.pendingSteer.length===1 && state.pendingSteer[0].id==='new','history projection prunes only exact landed ID');
state.cur={id:'another'}; state.msgs=[];
applyPending(mainView,{steer:[{id:'old',content:'same'}]});
assert(state.pendingSteer.length===1,'old session receipt must not affect new session');
`)
	})
	t.Run("response-after-message", func(t *testing.T) {
		interjectJS(t, `
response={accepted:true,id:'a',mode:'steer',pending:{steer:[{id:'a',content:'same'},{id:'b',content:'same'}]}};
hook=()=>handleEvent({type:'message',sessionId:'main',pendingId:'a',seq:4,message:{role:'user',content:'same'}},mainView);
sendMidrun('same',false,mainView,false).then(()=>{
 assert(state.pendingSteer.length===1 && state.pendingSteer[0].id==='b','HTTP snapshot after landing pruned by ID');
 response={accepted:true,id:'a',mode:'steer'};
 return sendMidrun('same',false,mainView,false);
}).then(()=>assert(state.pendingSteer.length===1,'legacy response cannot resurrect known landed ID')).catch(e=>{asyncError=e;});
`)
	})
}
