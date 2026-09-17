# Presenza client via WebSocket (Updater → API)

Design document — 2026-09-17

Lato client del canale descritto in
`emly-go-api/docs/superpowers/specs/2026-09-17-client-presence-ws-api-design.md`.
Quel documento è la fonte di verità del **protocollo** (endpoint, envelope,
handshake, heartbeat); questo descrive cosa fa l'Updater e perché.

---

## 0. Cosa deve fare l'Updater

Aprire una connessione WebSocket **in uscita** verso il server corrente della
catena (stesso concetto di `beginCycle`/`internal/service/sourcepolicy.go`
che governa già il poll manifest), mandare la propria identità al primo
messaggio del server, e restare lì — rispondendo a un ping ogni 10s — per
tutta la vita del servizio. Se cade, riconnettersi con backoff. Nient'altro:
oggi il canale non riceve comandi, solo il `hello` iniziale e gli heartbeat.

Diversamente dal relay VNC (`2026-09-08-vnc-relay-design.md`), qui la
connessione persistente **è la scelta**, non l'alternativa scartata: serve
presenza vera (online/offline nel momento in cui succede) e un canale aperto
per usi futuri (es. un trigger VNC senza aspettare il prossimo poll da
30s). Il costo — una connessione sempre aperta su ~350-400 macchine — è
accettato per questo.

## 1. Perché segue la stessa catena di server del poll manifest

Una macchina in sede parla con il mirror del suo sito, una fuori sede parla
col cloud — esattamente come il poll manifest oggi, senza nessuna
configurazione aggiuntiva. Il canale di presenza non ha una sua nozione di
"server giusto": legge lo stesso risultato che `beginCycle` produce per il
resto del ciclo (`cur`, lo stato dell'ultimo `beginCycle` in
`internal/service/service.go`). Se la site policy cambia server (rete
diversa, DC diverso), la connessione WS deve seguire: si chiude quella verso
il vecchio server e se ne apre una verso il nuovo, con lo stesso backoff di
una riconnessione normale.

## 2. Il ciclo

```
avvio servizio (RunLoop, dopo initPolicy):
  se clientWs.enabled (documento remoto) ──► avvia goroutine wsclient

wsclient (per la vita del processo):
  loop:
    server := server corrente della catena (da cur/snapshot)
    wss {server}/v2/client/ws     X-Api-Key
      │
      ├─ 401 ─────────────────────────► chiave sbagliata: logga, riprova
      │                                  al prossimo giro di backoff
      ├─ 404 sull'upgrade ────────────► questo server non implementa
      │                                  l'endpoint: smetti di riprovarci
      │                                  su QUESTO server, logga una volta
      │                                  (evento 922), riprova solo se
      │                                  beginCycle cambia server
      ├─ errore di rete/timeout ──────► backoff esponenziale, riprova
      │
      └─ upgrade ok
           riceve { "type": "hello" }
           manda { "type": "identity", "data": {...} }
           loop heartbeat:
             riceve { "type": "ping" } ──► manda { "type": "pong" }
             connessione persa ──► esci dal loop heartbeat, torna
                                    all'inizio (nuovo server corrente,
                                    backoff azzerato se la connessione
                                    era rimasta su per almeno N secondi)

kill switch remoto (clientWs.enabled → false a caldo):
  chiude la connessione corrente, ferma la goroutine, nessun retry finché
  una nuova revisione non lo riaccende
```

La convenzione del `404` è la stessa già documentata per manifest e per il
poll VNC: un server/mirror non ancora aggiornato non conosce
`/v2/client/ws`. Trattarlo come "riprova" farebbe generare un errore ogni
ciclo di backoff per sempre su ogni macchina di quel sito; il `404` sul
tentativo di upgrade disabilita il canale su quel server specifico finché
`beginCycle` non ne sceglie un altro (o il servizio riparte).

## 3. Backoff

Riusa la stessa idea del `updater.resolver`/`dcLookupRetry` già nel
documento remoto (`internal/policy/document.go`): esponenziale con cap,
azzerato quando una connessione resta stabile per un tempo minimo (es. 60s),
così una macchina che flappa per un attimo non finisce a fare retry ogni
30s per il resto della giornata. Nessun jitter necessario a questa scala
(~350-400 macchine): non è un thundering herd contro un endpoint costoso, è
un `Accept` su una route che non fa query.

Non è un ciclo indipendente scollegato dal resto: la selezione del server
resta quella di `beginCycle`, solo il timer di retry/riconnessione è suo.

## 4. Identità inviata

Stesso set di campi che `internal/source/http.go` (`HTTPSource.applyHeaders`)
manda oggi come header `X-EMLy-*` per manifest/download, qui dentro il
payload JSON di `identity` (§3.1 dello spec API):

```jsonc
{
  "hwid": s.HWID,
  "hostname": s.Hostname,
  "ad_domain": s.ADDomain,
  "logged_user": s.LoggedUser,
  "logged_user_state": s.LoggedUserState,
  "logged_user_disconnected_at": s.LoggedUserDisconnectedAt,  // RFC3339, solo se non zero
  "serial": s.Serial,
  "product": s.Product,
  "os_version": s.OSVersion,
  "emly_version": s.EMLyVersion
}
```

Stessa fonte dati di `HTTPSource` (`internal/machineinfo`), stessa semantica
di "campo assente = non riportato" già documentata lì. `X-EMLy-IntIP` non ha
equivalente qui: è specifico del manifest/download HTTP e resta tale.
`updater_version`/`contact` non sono nel JSON — vanno nello User-Agent della
richiesta di upgrade, come per ogni altra chiamata di questo updater
(`s.UserAgent`).

L'identity è risolta **una sola volta**, al momento dell'upgrade (stesso
istante in cui `HTTPSource` la risolverebbe per un manifest check) — non a
ogni `pong`. Un cambio di utente loggato durante la vita della connessione
si vede sul prossimo manifest check o alla prossima riconnessione WS, non in
tempo reale su questo canale: coerente con §3.2/§6 dello spec API (il canale
di presenza non sostituisce la telemetria del poll manifest).

## 5. Documento remoto

Stessa sezione descritta lato API, stesso pattern del kill switch `vnc`
proposto per il relay VNC:

```jsonc
"clientWs": {
  "enabled": false
}
```

`enabled: false` di default: aggiornare l'updater non apre nessuna
connessione finché un sito non pubblica una revisione che la accende.
Toccare questa sezione tocca `internal/policy/document.go`, `parse.go`,
`legacy.go` (default `enabled: false` per una macchina che non ha mai visto
un documento) e il validatore gemello in `emly-go-api/internal/remoteconfig`,
più le fixture `testdata/remoteconfig/` copiate verbatim nei due repo —
stessa lista di quattro posti che il design VNC documenta per la propria
sezione (§8.2 dello spec VNC).

## 6. Pacchetti

```
internal/wsclient/
  client.go     dial + handshake (hello → identity) + heartbeat loop
  backoff.go    §3: esponenziale con cap, reset su connessione stabile
internal/service/
  clientws.go   avvio/stop dentro RunLoop, letto dallo snapshot/cur,
                segue il server corrente della catena, spegnimento a caldo
                quando la revisione disabilita clientWs
```

`internal/wsclient` resta puro Go testabile senza API Windows, come
`internal/vnc/bridge.go`/`poll.go` proposti per il relay VNC; `clientws.go`
è il solo punto che tocca `RunLoop`/`cur`.

## 7. Eventi di log

Nella numerazione esistente (`internal/logging`), blocco 92x — il 91x è già
riservato al relay VNC (`2026-09-08-vnc-relay-design.md` §8):

| Evento | Quando |
|---|---|
| 920 | connessione stabilita (identity accettata) |
| 921 | connessione persa (motivo: rete, server, chiusura remota) |
| 922 | endpoint non implementato dal server corrente (404 sull'upgrade), una volta per server |
| 923 | canale disattivato da documento remoto (`clientWs.enabled` → false), disconnessione volontaria |

Un evento per ogni cambio di stato, non uno per ogni reconnect di backoff:
`921` non si ripete a ogni tentativo fallito dentro lo stesso ciclo di retry,
solo quando una connessione che era effettivamente su cade.

## 8. Checklist implementativa

- [ ] `internal/wsclient/client.go` + test (handshake, ping/pong, chiusura da
      entrambi i lati)
- [ ] `internal/wsclient/backoff.go` + test (esponenziale, cap, reset su
      connessione stabile)
- [ ] `internal/service/clientws.go`: avvio dal `RunLoop`, segue il server
      corrente, spegnimento a caldo su `clientWs.enabled=false`
- [ ] sezione `clientWs` in `internal/policy` (`document.go`, `parse.go`,
      `legacy.go`) + fixture condivise con l'API
- [ ] eventi 920-923 in `internal/logging`
- [ ] `AGENTS.md`: il pacchetto `internal/wsclient` e la convenzione del
      `404` sull'upgrade
