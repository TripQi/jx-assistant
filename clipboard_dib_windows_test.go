//go:build windows

package main

import (
 "encoding/binary"
 "fmt"
 "testing"
)

func TestDIBIncompleteAndBudget(t *testing.T) {
 for _, sz := range []uint32{40,52,56,108,124} {
  t.Run(fmt.Sprint(sz),func(t *testing.T) {
   b:=buildDIB(40,1,1,32,3,nil,nil)
   binary.LittleEndian.PutUint32(b,sz)
   if p,err:=dibToPNG(b); p!=nil || err!=errDIBIncomplete { t.Fatalf("bytes=%d err=%v",len(p),err) }
  })
 }
 for _, dims := range [][2]int{{0,1},{-1,1},{1,0},{1,-2147483648},{2147483647,1},{20000,20000},{4097,4096}} {
  t.Run(fmt.Sprint(dims),func(t *testing.T) {
   b:=buildDIB(40,dims[0],dims[1],32,0,nil,nil)
   p,err:=dibToPNG(b)
   want:=errDIBDimensions
   if dims[0]>0 && dims[0]<=20000 && dims[1]>0 && dims[1]<=20000 { want=errDIBBudget }
   if p!=nil || err!=want { t.Fatalf("bytes=%d err=%v want=%v",len(p),err,want) }
  })
 }
 b:=buildDIB(40,2,2,24,0,nil,[][]byte{{0,0,255}})
 if _,err:=dibToPNG(b); err!=errDIBIncomplete { t.Fatalf("truncated pixels: %v",err) }
}
