package httpapi

import (
	"net/http"

	docs "blinkpredict/banckend/docs"
)

func (s *Server) handleOpenAPISpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(docs.SwaggerInfo.ReadDoc()))
}

func (s *Server) handleOpenAPIDocs(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/swagger/index.html", http.StatusTemporaryRedirect)
}
