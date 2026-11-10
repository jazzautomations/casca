# Site do casca

Página estática de https://casca.jazzautomations.com.br. Sem framework e sem build.
A página é um man page: `index.html` (português), `en.html` (inglês) e `site.css`. Sem JavaScript.

    cd site && python3 -m http.server 8080

Publicação: a Vercel (projeto `casca`, Root Directory = `site`) publica a cada push na `main`.
