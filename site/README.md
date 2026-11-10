# Site do casca

Página estática de https://casca.jazzautomations.com.br. Sem framework e sem build.
`styles.css`, `boot.js` e `app.js` vêm do site do [jazz-cli](https://github.com/jazzautomations/jazz-cli/tree/main/site)
(mesma família visual); `casca.css` tem o pouco que é só daqui.

    cd site && python3 -m http.server 8080
    npx vercel@latest deploy --prod      # projeto "casca" na Vercel
