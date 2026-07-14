package service

import (
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func parallelGinTest(t *testing.T) {
	t.Helper()
	t.Parallel()
}
