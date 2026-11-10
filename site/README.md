# Site do casca

Página estática de https://casca.jazzautomations.com.br. Sem framework e sem build.
`styles.css`, `boot.js` e `app.js` vêm do site do [jazz-cli](https://github.com/jazzautomations/jazz-cli/tree/main/site)
(mesma família visual); `casca.css` tem o pouco que é só daqui.

    cd site && python3 -m http.server 8080

Publicação: a Vercel (projeto `casca`, Root Directory = `site`) publica a cada push na `main`.
