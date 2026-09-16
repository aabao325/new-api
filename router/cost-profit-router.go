package router

import (
	"github.com/QuantumNous/new-api/costprofit"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

// registerCostProfitRoutes mounts the channel cost and profit panel under its
// own /cost-profit namespace.
//
// This file and one line in api-router.go are the panel's entire footprint in
// the router package, so upstream route changes cannot conflict with it.
//
// Authorization is split by what the endpoint can do rather than by which page
// shows it:
//
//   - RootAuth for anything that writes. Cost configuration, attribution,
//     approvals, payouts and recompute all decide how money is divided, so they
//     stay with the site owner even though managers can see their results.
//   - AdminAuth for reads. Managers already see channels upstream, so channel
//     costs are not new information to them, and the group cost dashboard is
//     something they are meant to act on.
//
// Endpoints that return a manager's own figures additionally filter by the
// caller's id inside the handler. That filter is server-side on purpose: a
// manager must not be able to widen their scope by editing a request, and a
// frontend-only filter would let them.
func registerCostProfitRoutes(apiRouter *gin.RouterGroup) {
	costProfitRoute := apiRouter.Group("/cost-profit")
	costProfitRoute.Use(middleware.AdminAuth())
	{
		costProfitRoute.GET("/status", costprofit.GetStatus)
	}
}
