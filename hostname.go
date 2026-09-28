package main

import (
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

// dockerContainerID matches the hostname Docker gives a container that was not
// given one: the first 12 hex digits of its ID, or all 64 on some runtimes.
var dockerContainerID = regexp.MustCompile(`^[0-9a-f]{12}$|^[0-9a-f]{64}$`)

// pageName is what the web pages call this instance in place of "AH4C": the
// container's hostname, so someone running ah4c, ah4c2 and ah4c3 can tell
// their browser tabs apart. It is empty, and the pages keep "AH4C", when the
// hostname is Docker's default, since a container ID names nothing a person
// chose.
func pageName(hostname string) string {
	hostname = strings.TrimSpace(hostname)
	if dockerContainerID.MatchString(hostname) {
		return ""
	}
	return hostname
}

func registerHostnameRoutes(r *gin.Engine) {
	r.GET("/api/hostname", func(c *gin.Context) {
		hostname, _ := os.Hostname()
		c.JSON(http.StatusOK, gin.H{"name": pageName(hostname)})
	})
}
