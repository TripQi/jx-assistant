package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// 奇数额度的 80% 按上取整判断；100% 必须优先，拒写必须原子。
func TestProjectKnowledgeOddBudgetBoundaries(t *testing.T) {
	for _, limit := range []int64{1, 3, 7, 11} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			s := projectTestStore(t)
			p := projectTestCreate(t, s, "odd")
			k := projectTestKnowledge(t, s, p.ID)
			threshold := (limit*4 + 4) / 5
			for used := int64(0); used <= limit; used++ {
				warning := projectKnowledgeWarning(used, limit)
				switch {
				case used == limit:
					if !strings.Contains(warning, "100%") {
						t.Fatalf("used=%d limit=%d must report 100%%: %q", used, limit, warning)
					}
				case used >= threshold:
					if !strings.Contains(warning, "80%") {
						t.Fatalf("used=%d limit=%d must report 80%%: %q", used, limit, warning)
					}
				default:
					if warning != "" {
						t.Fatalf("below ceil(80%%): used=%d limit=%d warning=%q", used, limit, warning)
					}
				}
				var err error
				k, err = s.SaveProjectKnowledge(p.ID, k.Revision, strings.Repeat("a", int(used*4)), &limit, projectTestAllow)
				if err != nil || ProjectKnowledgeBudget(k.Content) != used {
					t.Fatalf("exact budget %d/%d: %+v %v", used, limit, k, err)
				}
			}
			before := k
			_, err := s.SaveProjectKnowledge(p.ID, k.Revision, strings.Repeat("a", int(limit*4+1)), nil, projectTestAllow)
			var over *ProjectKnowledgeBudgetError
			if !errors.As(err, &over) || over.Used != limit+1 || over.Limit != limit {
				t.Fatalf("overflow: %v", err)
			}
			if got := projectTestKnowledge(t, s, p.ID); got != before {
				t.Fatalf("overflow mutated content/revision/time: %+v != %+v", got, before)
			}
		})
	}
}
