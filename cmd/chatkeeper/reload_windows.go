package main

import (
	"context"
	"github.com/bitskyai/chatkeeper/internal/web"
)

func watchReload(context.Context, *web.App) func() { return func() {} }
