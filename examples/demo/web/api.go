package web

import "net/http"

//fabrik:http:group /api
type API struct {
	Greeter Greeter
}

//fabrik:http GET /greet/{name}
func (a *API) Greet(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(a.Greeter.Greet(r.PathValue("name"))))
}
