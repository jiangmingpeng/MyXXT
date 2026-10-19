package common

import "github.com/gin-gonic/gin"

type APIResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data"`
}

func Success(c *gin.Context, data interface{}) {
	c.JSON(200, APIResponse{Code: 0, Message: "ok", Data: data})
}

func Fail(c *gin.Context, status int, message string) {
	c.JSON(status, APIResponse{Code: CodeFail, Message: message, Data: nil})
}

// FailWithCode 允许返回业务错误码（如二维码失效），用于前端按 code 做差异化处理。
func FailWithCode(c *gin.Context, status int, code int, message string) {
	c.JSON(status, APIResponse{Code: code, Message: message, Data: nil})
}
