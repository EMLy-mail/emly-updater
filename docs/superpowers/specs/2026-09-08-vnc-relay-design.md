# Accesso remoto VNC via relay (Updater → API → dashboard)

Design document — 2026-09-08

Lato client del relay descritto in
`emly-go-api/docs/superpowers/specs/2026-09-08-vnc-relay-api-design.md`.
Quel documento è la fonte di verità del protocollo (endpoint, token, stati,
codici di risposta); questo descrive cosa fa l'Updater e perché.

---

## 0. Cosa deve fare l'Updater

TightVNC è già installato e configurato sulla macchina, in ascolto su
`127.0.0.1:5900`. L'operatore, dalla dashboard, chiede una sessione. Il
compito dell'Updater è, in ordine:

1. accorgersi che c'è una sessione in attesa per **questa** macchina;
2. chiedere il consenso all'utente che sta davanti al PC;
3. aprire una WebSocket **in uscita** verso l'API e collegarla al TCP locale
   di TightVNC;
4. chiudere e riportare com'è andata.

L'Updater non parla RFB e non lo interpreta: copia byte fra una WebSocket e
un socket TCP. Il protocollo VNC resta una conversazione fra TightVNC e
noVNC, con l'API e l'Updater come tubi.

## 1. Perché in uscita, e perché a poll

La macchina è dietro NAT: nessuno da fuori può aprire una connessione verso
di lei. Vale per l'API in cloud tanto quanto per un attaccante, ed è una
proprietà che vogliamo **tenere**. Quindi la connessione parte sempre da
qui.

Ma se nessuno può chiamare la macchina, la macchina deve chiedere. Le
opzioni erano una WebSocket di controllo permanente (latenza 1-2s, ma una
connessione aperta per sempre su ogni macchina della flotta, con backoff,
riconnessione e keepalive attraverso i firewall dei clienti — un ciclo di
vita che questo servizio oggi non ha da nessuna parte) e un poll breve.

**Scelto il poll a 30s.** L'Updater è già un servizio a poll, fail-open, che
non tiene niente di aperto: aggiungere un timer è aggiungere niente.
L'operatore aspetta al massimo mezzo minuto fra il click e la schermata. Se
il PoC dirà che è troppo, il protocollo non cambia: si aggiunge solo il
canale di risveglio.

## 2. Il ciclo

```
ogni vnc.pollInterval (default 30s), solo se vnc.enabled:

  GET {server}/v2/vnc/pending          X-Api-Key, X-EMLy-HWID
    │
    ├─ 204 ─────────────────────────► niente da fare (il 99,99% dei casi)
    ├─ 404 ─────────────────────────► questo server non implementa il relay:
    │                                  smetti di pollarlo, logga una volta
    ├─ errore/timeout ──────────────► ignora, riprova al prossimo giro
    └─ 200 {session_id, agent_token, target, requested_by, consent}
         │
         ├─ consenso negato o scaduto ─► POST /v2/vnc/sessions/{id}/deny
         │
         └─ consenso ok
              wss {server}/v2/vnc/agent   X-Api-Key, X-VNC-Agent-Token
              net.Dial tcp target
              io.Copy nei due sensi finché uno dei due chiude
```

Il `404` è il punto sottile, ed è la stessa convenzione del manifest
updater: un mirror di sito non ancora aggiornato non conosce
`/v2/vnc/pending` e risponde `404`. Se lo trattassimo come "riprova", ogni
macchina di quel sito genererebbe un errore ogni 30 secondi per sempre. `404`
significa "questa funzione non esiste qui": si disabilita il poll fino al
prossimo cambio di server (o riavvio del servizio) e si logga una riga sola.
"Nessuna sessione in attesa" è `204`, che non logga niente.

Il poll usa il **server corrente della catena** decisa da `beginCycle`
(`internal/service/sourcepolicy.go`), non un URL suo: una macchina in sede
parla con il mirror del suo sito, una fuori sede parla con il cloud, senza
nessuna configurazione aggiuntiva.

## 3. Il bridge

Il pezzo di codice utile è tutto qui. `coder/websocket` — la stessa libreria
che l'API usa per lo stats stream — espone `websocket.NetConn`, che dà un
`net.Conn` sopra la WebSocket:

```go
c, _, err := websocket.Dial(ctx, wssURL, &websocket.DialOptions{
    HTTPHeader: http.Header{
        "X-Api-Key":         {apiKey},
        "X-VNC-Agent-Token": {agentToken},
    },
})
if err != nil { return err }
defer c.CloseNow()

ws := websocket.NetConn(ctx, c, websocket.MessageBinary)

tcp, err := net.DialTimeout("tcp", target, 5*time.Second)
if err != nil { return err }   // TightVNC non è in ascolto
defer tcp.Close()

go func() { io.Copy(ws, tcp) }()   // VNC → dashboard
io.Copy(tcp, ws)                   // dashboard → VNC
```

`MessageBinary` non è un dettaglio: RFB è binario, e un frame di testo
romperebbe il flusso appena passa un byte non UTF-8.

Valutato e scartato `github.com/evangwt/go-vncproxy`: è un `http.Handler`
che *riceve* la WebSocket e fa lui la `Dial` verso il VNC — il ruolo
opposto a quello che serve qui — e porterebbe `gorilla/websocket` come
seconda libreria WebSocket nei due repo per sostituire le venti righe qui
sopra.

## 4. Consenso

La macchina è di un cliente e davanti c'è una persona che sta lavorando.
Prima di aprire il tunnel, `internal/notify` mostra il dialog nella sessione
dell'utente console — lo stesso hop SYSTEM→utente che l'avviso di update
critico usa già (`internal/notify/console_user.go`). Il testo dice chi ha
chiesto l'accesso (`requested_by`, che l'API prende dalla sessione admin).

Tre modalità, decise **dal documento remoto** e quindi per sito:

- `required` (default) — senza un sì esplicito non si apre niente. Silenzio
  per `consentTimeout` = rifiuto.
- `notify` — avvisa e procede: per parchi macchine dove il consenso è già
  coperto contrattualmente.
- `none` — nessun prompt: macchine non presidiate (chioschi, sale riunioni,
  server di sala).

Nessuna sessione utente attiva (macchina al login screen) con `required`
significa rifiuto, non "procedi": è la risposta prudente, e l'operatore la
vede come tale sulla dashboard.

## 5. TightVNC su loopback

`internal/vnc` fa self-heal del registro alla partenza del servizio, nello
stile di `internal/assoc`: `HKLM\SOFTWARE\TightVNC\Server`,
`AllowLoopback=1` e `LoopbackOnly=1`.

Non è un dettaglio di configurazione, è il modello di sicurezza. Con
TightVNC su loopback, **l'unica via d'ingresso al desktop è il tunnel
autenticato**: nessuna porta 5900 raggiungibile sulla LAN del cliente,
nessuna password VNC come unica difesa, nessuna superficie in più rispetto a
oggi. Se il relay è spento, la macchina è esattamente com'era prima.

## 6. Documento remoto

Sezione nuova, con gli stessi override per host/sito di tutto il resto:

```jsonc
"vnc": {
  "enabled": false,          // kill switch: default spento
  "target": "127.0.0.1:5900",
  "pollInterval": "30s",
  "consent": "required",     // required | notify | none
  "consentTimeout": "60s"
}
```

`enabled: false` di default significa che aggiornare l'updater non abilita
niente: l'accesso remoto si accende sito per sito, pubblicando una revisione.
E poiché il documento è validato all-or-nothing e cachato, la macchina non
può finire in uno stato in cui il VNC è acceso "per sbaglio" da una risposta
malformata.

Toccare questa sezione tocca `internal/policy/document.go`, `parse.go`,
`legacy.go` (dove il default è `enabled: false`, così una macchina che non ha
mai visto un documento non espone niente), il validatore gemello in
`emly-go-api/internal/remoteconfig`, **e** le fixture `testdata/remoteconfig/`
copiate verbatim nei due repo.

## 7. Pacchetti

```
internal/vnc/
  bridge.go     WS↔TCP: la funzione del §3, più i contatori di byte
  poll.go       il ciclo del §2 sopra source.HTTPSource
  consent.go    prompt WTS + le tre modalità
  tightvnc.go   self-heal del registro (loopback-only), rilevamento servizio
internal/service/
  vnc.go        avvio/stop del poll dentro RunLoop, letto dallo snapshot
```

`bridge.go` e `poll.go` restano puro Go testabile senza API Windows, come il
resto dei test di questo repo; `consent.go` e `tightvnc.go` sono l'unica
parte Windows-only.

## 8. Eventi di log

Nella numerazione esistente (`internal/logging`), blocco 91x:

| Evento | Quando |
|---|---|
| 910 | sessione reclamata (id, chi l'ha chiesta) |
| 911 | consenso negato / scaduto |
| 912 | tunnel aperto |
| 913 | tunnel chiuso (durata, byte nei due sensi) |
| 914 | TightVNC non raggiungibile su `target` |
| 915 | relay non implementato dal server corrente (una volta per server) |

Un accesso al desktop di un utente deve lasciare traccia **sulla macchina**,
non solo nel database dell'API: l'Event Log locale è quello che un cliente
può leggere da sé.

## 9. PoC

Primo passo, per misurare latenza e banda reali prima di costruire il resto:
un sottocomando `vnc-agent` in foreground (accanto a `run`) che polla e fa da
bridge, senza consenso, senza documento remoto, senza toccare il ciclo del
servizio. Serve a rispondere a una domanda sola: 30 secondi di attesa e la
banda attraverso il cloud sono accettabili, o serve il canale di risveglio
persistente?

## 10. Checklist implementativa

- [ ] `internal/vnc/bridge.go` + test (chiusura da entrambi i lati, target
      irraggiungibile)
- [ ] `internal/vnc/poll.go` + test sui codici 200/204/404/errore
- [ ] `internal/vnc/consent.go` (tre modalità, nessuna sessione console)
- [ ] `internal/vnc/tightvnc.go` (self-heal loopback-only)
- [ ] sezione `vnc` in `internal/policy` + fixture condivise con l'API
- [ ] avvio dal `RunLoop` guidato dallo snapshot, spegnimento a caldo quando
      la revisione lo disabilita
- [ ] eventi 910-915 in `internal/logging`
- [ ] `AGENTS.md`: il pacchetto `internal/vnc` e la convenzione del `404`
