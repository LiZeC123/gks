package policy

import (
	"context"
	"log/slog"

	thoth "github.com/expr-thoth/expr-thoth"
	"github.com/expr-thoth/expr-thoth/core"
)

var thothIns *thoth.Thoth
var logger = slog.Default()

func init() {
	thothIns = thoth.NewThoth()
	LoadFileSystemPolicys(thothIns, "./policy")
}

func GetCheck(ctx context.Context, req core.Request) (*core.Response, error) {
	return thothIns.Execute(ctx, req)
}
