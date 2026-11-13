package main

import (
	"context"
	"strings"
	"testing"
)

// 出厂不预置通道后，「没接公司通道也没自己加通道」的机器上发送消息必命中
// ErrNoModelChannelText：前端靠这段文案挂「接入公司通道」按钮（见
// web/app-chat.js 的 NO_CHANNEL_HINT），所以这里把文案也钉住。
func TestStreamChatWithoutChannel(t *testing.T) {
	err := StreamChat(context.Background(), LLMTarget{}, ChatRequest{}, StreamCallbacks{})
	if err == nil {
		t.Fatal("没有通道时应直接报错，不该去请求空地址")
	}
	if err.Error() != ErrNoModelChannelText {
		t.Fatalf("报错文案被改动了：%q（前端 NO_CHANNEL_HINT 依赖它，改要两边一起改）", err.Error())
	}
	if !strings.Contains(err.Error(), "接入公司通道") {
		t.Fatalf("报错里应提示「接入公司通道」按钮：%q", err.Error())
	}
}
