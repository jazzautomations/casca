# Site do casca

Página estática de https://casca.jazzautomations.com.br. Sem framework e sem build.
`index.html`, `site.css` e `site.js` são escritos à mão. A janela no topo da página é uma imitação
em HTML (`site.js`), não um print.

    cd site && python3 -m http.server 8080

Publicação: a Vercel (projeto `casca`, Root Directory = `site`) publica a cada push na `main`.
