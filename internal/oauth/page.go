package oauth

import (
	"html/template"
	"net/http"
)

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect Odoo</title>
<style>
  :root { --bg:#f6f7f9; --card:#fff; --text:#1d2433; --muted:#5b6475; --border:#d8dce3; --accent:#714b67; --error:#b42318; }
  @media (prefers-color-scheme: dark) { :root { --bg:#14161a; --card:#1d2026; --text:#e8eaee; --muted:#a3aab8; --border:#343944; --accent:#c79bbd; --error:#f97066; } }
  * { box-sizing: border-box; }
  body { margin:0; background:var(--bg); color:var(--text); font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif; }
  main { max-width:420px; margin:48px auto; padding:0 16px; }
  .card { background:var(--card); border:1px solid var(--border); border-radius:12px; padding:28px; }
  h1 { font-size:20px; margin:0 0 4px; }
  p.sub { color:var(--muted); margin:0 0 20px; }
  label { display:block; font-weight:600; margin:14px 0 6px; }
  input { width:100%; padding:10px 12px; border:1px solid var(--border); border-radius:8px; background:transparent; color:var(--text); font:inherit; }
  button { width:100%; margin-top:20px; padding:11px; border:0; border-radius:8px; background:var(--accent); color:#fff; font:inherit; font-weight:600; cursor:pointer; }
  .error { color:var(--error); margin:0 0 12px; }
  details { margin-top:20px; color:var(--muted); }
  summary { cursor:pointer; }
  ol { padding-left:20px; }
</style>
</head>
<body>
<main>
  <div class="card">
    <h1>Connect Odoo</h1>
    <p class="sub">Your AI assistant (Claude or ChatGPT) will see exactly what your Odoo account can see, read-only.</p>
    {{if .Error}}<p class="error" role="alert">{{.Error}}</p>{{end}}
    <form method="post" action="/authorize">
      <input type="hidden" name="response_type" value="code">
      <input type="hidden" name="client_id" value="{{.Req.ClientID}}">
      <input type="hidden" name="redirect_uri" value="{{.Req.RedirectURI}}">
      <input type="hidden" name="state" value="{{.Req.State}}">
      <input type="hidden" name="code_challenge" value="{{.Req.Challenge}}">
      <input type="hidden" name="code_challenge_method" value="S256">
      <label for="login">Odoo email</label>
      <input id="login" name="login" type="email" autocomplete="username" required value="{{.Login}}">
      <label for="api_key">Odoo API key</label>
      <input id="api_key" name="api_key" type="password" autocomplete="off" required>
      <button type="submit">Connect</button>
    </form>
    <details>
      <summary>How do I get an API key?</summary>
      <ol>
        <li>In Odoo, click your avatar (top right) → <b>My Profile</b>.</li>
        <li>Open the <b>Account Security</b> tab → <b>New API Key</b>.</li>
        <li>Confirm your password, name it "AI connector", and copy the key.</li>
      </ol>
      <p>Delete the key there at any time to disconnect.</p>
    </details>
  </div>
</main>
</body>
</html>`))

var errorTmpl = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connection error</title>
<style>body{margin:0;background:#f6f7f9;color:#1d2433;font:15px/1.5 system-ui,sans-serif}main{max-width:420px;margin:48px auto;padding:0 16px}
@media (prefers-color-scheme: dark){body{background:#14161a;color:#e8eaee}}</style></head>
<body><main><h1>Can't start sign-in</h1><p>{{.}}</p><p>Start again from the Connect button in Claude or ChatGPT.</p></main></body></html>`))

func renderLogin(w http.ResponseWriter, req authRequest, login, errMsg string) {
	setPageHeaders(w)
	loginTmpl.Execute(w, struct {
		Req          authRequest
		Login, Error string
	}{req, login, errMsg})
}

func renderError(w http.ResponseWriter, msg string) {
	setPageHeaders(w)
	w.WriteHeader(http.StatusBadRequest)
	errorTmpl.Execute(w, msg)
}

func setPageHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY") // no clickjacking of the credential form
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
}
