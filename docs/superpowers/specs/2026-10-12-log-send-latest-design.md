# `log.send_latest` — log dell'Updater via canale client (Updater → API → Dashboard)

Design document — 2026-10-12

Un nuovo comando del protocollo v2 (`GET /v2/client/ws`) con cui la
dashboard chiede a una macchina connessa il proprio log corrente
(`%ProgramData%\EMLyUpdater\logs\updater.log`), per diagnosticarla senza
accesso remoto alla macchina.

Implementato in tutti e tre i repo, ciascuno su un branch
`feat/log-send-latest`. §3 è l'Updater, §4 l'API e §5 la dashboard. Il
riferimento normativo del protocollo resta `emly-go-api/CLIENT_WS_PROTOCOL.md`,
che ora contiene il testo di §2.

---

## 0. Cosa fa

```
Dashboard ──POST /v2/client/{id}/commands {"name":"log.send_latest"}──► API
API ──command log.send_latest──► Updater
Updater ──ack──► API
Updater: legge la coda di updater.log, gzip, base64, taglia per stare in un frame
Updater ──result {LogFile}──► API
API: decodifica, valida, salva .log.gz su S3 + riga in MySQL
Dashboard ◄── GET /v2/client/commands/{id} (polling, come oggi) / GET .../logs
```

Solo lettura: niente sulla macchina cambia. Nessun argomento.

## 1. Decisioni

| Decisione | Scelta | Perché |
|---|---|---|
| Nome sul filo | `log.send_latest` | Il catalogo usa `namespace.verbo_con_underscore` (`apps.list_upgradable`, CLIENT_WS_PROTOCOL.md §3). "sendLatestLogFile" in camelCase sarebbe l'unico nome fuori schema. |
| Trasporto | Un solo frame `result` (≤ 64 KiB) | Nessun endpoint nuovo né credenziali per un upload HTTP, nessun chunking da riassemblare, stessa dedupe/ack/timeout degli altri comandi. |
| Cosa entra | La **coda** del file, tagliata all'inizio di una riga | Per una diagnosi servono le righe più recenti. Con un rapporto gzip di ~11:1 su un log reale, un frame contiene ~450-500 KB di log: circa un quarto del file da 2 MB di default, ore o giorni di attività. |
| File | Solo `updater.log`, non i `.log.gz` ruotati | Un frame non ha comunque spazio per altro. |
| Policy | **Opt-in**: non in `DefaultClientWSCommands` | È in sola lettura, ma porta fuori dalla macchina hostname, utenti, IP e URL dei server. Va nominato nel documento in `clientWs.commands`. |
| Trasporto TLS | Non richiesto (come `machine.info`) | `machine.info` porta già gli stessi dati in chiaro su `ws://`. Solo i comandi distruttivi richiedono `wss://` (§12.2). |
| Banda | Al massimo ~60 KB per comando, uno alla volta per macchina | È un'azione puntuale di un admin. Non esiste un "manda il log a tutta la flotta": la dashboard lo lancia su una macchina sola (§5.3). Con un link MPLS stretto un fan-out è proprio quello da non costruire. |

## 2. Protocollo (testo per CLIENT_WS_PROTOCOL.md)

### 2.1 §1 Casi d'uso — riga nuova

| Log dell'Updater | `command` `log.send_latest` → `ack` + `result` | S→C, C→S | `internal/logging.ReadTail` |

### 2.2 §6.6 `LogFile`

```json
{
  "name": "updater.log",
  "size_bytes": 1834221,
  "modified_at": "2026-10-12T08:14:59Z",
  "offset": 1371904,
  "included_bytes": 462317,
  "level": "info",
  "encoding": "gzip+base64",
  "content": "H4sIAAAAAAAC/+y9a3MbR5Yo+H1+...",
  "collected_at": "2026-10-12T08:15:02Z"
}
```

| Campo | Tipo | Significato |
|---|---|---|
| `name` | string | Nome del file letto (oggi sempre `updater.log`). |
| `size_bytes` | int | Dimensione del file al momento della lettura. |
| `modified_at` | RFC 3339 | Ultima scrittura del file. Omesso se sconosciuto. |
| `offset` | int | Byte del file da cui parte `content`. `0` = file intero. È sempre l'inizio di una riga. |
| `included_bytes` | int | Byte di log (non compressi) in `content`. `offset + included_bytes = size_bytes`. |
| `level` | string | Livello di log attivo sul servizio (`debug`/`info`/`warn`/`error`): dice cosa *non* può esserci nel file. |
| `encoding` | string | Sempre `gzip+base64`: gzip (RFC 1952) dei byte inclusi, poi base64 standard con padding. |
| `content` | string | Il log, codificato come dice `encoding`. |
| `collected_at` | RFC 3339 | Quando l'Updater ha letto il file. |

`result.truncated` è `true` esattamente quando `offset > 0`.

### 2.3 §7 Catalogo — riga nuova

| `log.send_latest` | — | `result` `LogFile` | 30s | lettura (opt-in) |

### 2.4 §7.7 `log.send_latest`

Legge la coda di `updater.log` (al massimo gli ultimi 2 MiB) e la comprime con
gzip. Se il `result` non sta in §11, scarta righe **dall'inizio** finché non ci
sta, così le righe più recenti sono sempre incluse. `args` deve essere assente o
`{}`: un campo qualsiasi è `invalid_args`.

Errori specifici: `not_found` (il file non esiste: servizio appena installato,
log cancellato a mano), `internal` (lettura fallita). Un file vuoto non è un
errore: `content` è il gzip di zero byte.

### 2.5 §12.3 Policy

`log.send_latest` è in sola lettura ma **non** è nei default. Un documento che
non lo nomina lo rifiuta con `disabled_by_policy`.

## 3. Lato Updater (implementato)

| Dove | Cosa |
|---|---|
| `internal/wsclient/protocol.go` | `CmdLogSendLatest`, in `CommandNames`. `ValidateArgs` lo tratta come comando senza argomenti. Tipo `LogFile`, `LogEncodingGzipBase64`. |
| `internal/policy/document.go` | In `knownClientWSCommands`, **non** in `DefaultClientWSCommands`. |
| `internal/logging/tail.go` | `ReadTail`: apre il file con `FILE_SHARE_DELETE`, così la rotazione di lumberjack (una rename sul file aperto) non fallisce durante la lettura. `Logger.FilePath`. |
| `internal/service/clientlogs.go` | `sendLatestLog` e `fitLogTail`: ricerca binaria sugli inizi di riga per trovare la coda più lunga il cui payload JSON sta in `resultPayloadBudget` (60000). |
| `testdata/remoteconfig/valid/clientws-log-send-latest.json` | Fixture che nomina il comando: va copiata identica in `emly-go-api` (§4.2). |

La dedupe per id, il rifiuto `busy` di un secondo `log.send_latest` mentre il
primo gira e il controllo di `expires_at` sono quelli comuni a tutti i comandi
(`executeCommand`). Nessun evento nell'Event Log (è un comando di sola lettura,
come `machine.info`): una riga `client command accepted` nel file di log.

## 4. API (`emly-go-api`, implementato)

Branch `feat/log-send-latest` (worktree `emly-go-api.worktrees/log-send-latest`).
`CLIENT_WS_PROTOCOL.md` (§1, §6.6, §7.7, §10, §12.3, §13.1, §14.3), `ROUTES.md`
§5.9, `DOCS.md` e `CLAUDE.md` sono aggiornati nello stesso change.
`openapi.yaml` non documenta nessuna route `/v2/client`, quindi non è toccato.

### 4.1 `internal/clientproto`

- `CmdLogSendLatest = "log.send_latest"`, in `ServerCapabilities`. Un'API
  senza questa modifica lo scarta già da sola in `Intersect`: un Updater nuovo
  contro un'API vecchia funziona come prima, il comando semplicemente non è
  disponibile (`422`).
- `Commands[CmdLogSendLatest] = {ResultTimeout: 30 * time.Second}`: non
  distruttivo, esito via `result`.
- `ValidateArgs`: stesso ramo "nessun argomento" di `apps.list_upgradable`.
- Tipi `LogFile` e `LogEncodingGzipBase64`, identici a quelli dell'Updater.

### 4.2 Documento remoto (`internal/remoteconfig`)

- `"log.send_latest"` in `KnownClientWSCommands`. Il test
  `TestKnownClientWSCommandsMatchProtocol` lo tiene allineato a
  `clientproto.Commands`.
- Fixture `testdata/remoteconfig/valid/clientws-log-send-latest.json` copiata
  identica da questo repo. `defaults.json` resta invariato: il comando non è
  nei default.

### 4.3 Persistenza (`internal/clientlogs`, nuovo package)

I `CommandRecord` vivono solo in memoria (24h, persi al riavvio). Per i log
non basta, quindi c'è un package che li salva.

**Hook sul risultato.** `Hub.HandleResult` tiene `h.mu`, e un upload S3 lì
dentro bloccherebbe tutto il hub. Nuovo `Hub.OnResult(fn)`: `fn` viene
chiamata **in una goroutine propria**, con una copia del record, una volta per
ogni comando che finisce `done`. Una redelivery del `result` non la richiama,
perché `HandleResult` passa da `acked` a `done` una sola volta. `main.go`
registra `clientLogs.HandleResult`, che ignora tutto tranne `log.send_latest`.

**Validazione** (`decodeLogFile`). `content` arriva da una macchina e non è
fidato:
- `encoding` deve essere `gzip+base64`;
- base64 e gzip devono essere validi;
- decompresso non deve superare **4 MiB** (`io.LimitReader`): il client ne
  manda al più 2 MiB, quindi oltre il limite è una gzip bomb.

Se uno di questi controlli fallisce non viene scritta nessuna riga: resta un
warn `client log not saved`. Se la lunghezza decompressa è diversa da
`included_bytes`, il log viene salvato comunque con `mismatch = 1`, perché
serve proprio a diagnosticare.

**Dove finiscono i byte.** Il gzip così come è arrivato (niente
ricompressione) va nel bucket dei file API, con key
`emly-api-files/client-logs/{client_id}/{command_id}.log.gz`, quando c'è
(`USE_S3_API_FILE_STORAGE`). Senza bucket, o se l'upload fallisce, va nella
colonna `content`: è la stessa degradazione dei file dei bug report.

**MySQL**: migration `27_updater_client_logs.sql` (task
`27_updater_client_logs`, condizione `table_not_exists`):

```sql
CREATE TABLE IF NOT EXISTS `updater_client_logs` (
    `id`              CHAR(26)     NOT NULL,          -- = command id (ULID)
    `client_id`       INT UNSIGNED NOT NULL,
    `issued_by`       VARCHAR(255) NULL,
    `file_name`       VARCHAR(64)  NOT NULL,
    `size_bytes`      BIGINT       NOT NULL,
    `offset_bytes`    BIGINT       NOT NULL,
    `included_bytes`  BIGINT       NOT NULL,
    `truncated`       TINYINT(1)   NOT NULL,
    `mismatch`        TINYINT(1)   NOT NULL DEFAULT 0,
    `log_level`       VARCHAR(10)  NULL,
    `modified_at`     DATETIME     NULL,
    `collected_at`    DATETIME     NULL,              -- orologio del client
    `received_at`     DATETIME     NOT NULL,          -- orologio dell'API
    `gzip_bytes`      INT UNSIGNED NOT NULL,
    `updater_version` VARCHAR(20)  NULL,              -- snapshot all'arrivo
    `hostname`        VARCHAR(255) NULL,              -- snapshot all'arrivo
    `s3_key`          VARCHAR(255) NULL,
    `content`         MEDIUMBLOB   NULL,              -- solo senza bucket
    PRIMARY KEY (`id`),
    INDEX `idx_client_received` (`client_id`, `received_at`)
);
```

Non c'è una foreign key verso `updater_clients`, ed è voluto: un
`ON DELETE CASCADE` cancellerebbe le righe e lascerebbe orfani gli oggetti nel
bucket.

**L'id del log è l'id del comando.** Così la dashboard passa da un
`log.send_latest` concluso al suo log senza un campo in più nel
`CommandRecord`: niente `log_id`, al contrario della prima stesura.

**Retention**: `Store.Start` è un loop di `main.go`, come `eventprune`, e gira
subito e poi ogni ora. Cancella:
- tutto oltre gli ultimi **20 log per macchina**;
- tutto ciò che è più vecchio di **30 giorni**;
- tutti i log delle macchine rimosse da `updater_clients`.

Lavora a lotti di 500 (S3 `DeleteObjects` accetta al massimo 1000 chiavi) e
cancella **prima l'oggetto, poi la riga**: se S3 fallisce resta tutto com'era
e si riprova all'ora successiva.

`Store` non ha stato (solo `db` e il bucket): `main.go` (per hook e prune) e
`v2.NewRouter` (per le route) ne costruiscono uno a testa invece di
passarselo attraverso tutte le firme.

### 4.4 REST (admin)

Montate da `clientws.RegisterV2` nello stesso gruppo `AdminKeyAuth` di
comandi/eventi/notify, sotto `/v2/client`.

| Metodo | Path | Risposta | Codici |
|---|---|---|---|
| `GET` | `/v2/client/{client_id}/logs` | `{"logs": [ClientLogRecord, …]}`, più recenti prima, senza contenuto | `200` (lista vuota se nessuno); `400` |
| `GET` | `/v2/client/logs/{log_id}` | `ClientLogRecord` | `200`; `400`; `404` |
| `GET` | `/v2/client/logs/{log_id}/content` | Il log decompresso, `text/plain; charset=utf-8` | `200`; `400`; `404`; `502` bucket irraggiungibile |
| `GET` | `/v2/client/logs/{log_id}/content?format=gz` | Il gzip così come salvato, `application/gzip` | come sopra |
| `DELETE` | `/v2/client/logs/{log_id}` | — | `204`; `400`; `404` |

- `log_id` deve essere un ULID (26 caratteri Crockford base32), altrimenti
  `400`. Il controllo avviene prima di qualsiasi query o chiave S3.
- Senza database (test) tutte le route rispondono `503`.
- `content` va sempre come allegato:
  - `Content-Disposition: attachment; filename="<hostname>_<yyyyMMdd-HHmmss>_updater.log"`,
    con ogni carattere fuori da `[A-Za-z0-9._-]` sostituito da `_`, perché
    hostname e nome file vengono dalla macchina;
  - `X-Content-Type-Options: nosniff`;
  - `Cache-Control: private, no-store`.

### 4.5 Test

Senza un MySQL di test (il repo non ha sqlmock), le query non sono coperte. È
coperto tutto il resto:
- `clientlogs`:
  - `decodeLogFile`: payload valido, `mismatch`, encoding sbagliato, base64
    rotto, gzip rotto, gzip bomb;
  - `attachmentName`: virgolette e path traversal;
  - `validID`;
  - `pruneVictims`: cap per macchina, retention, macchina cancellata;
  - route: `400` sugli id non validi e `503` senza database.
- `clienthub`: `OnResult` scatta una volta sola, non su un `failed` e non su
  una redelivery; hub nil.
- `clientproto`: argomenti, `ResultTimeout`, `CommandSpec`.

## 5. Dashboard (`emly-dashboard-react`, implementato)

Branch `feat/log-send-latest` (worktree
`emly-dashboard-react.worktrees/log-send-latest`).

### 5.1 `lib/api.ts`

- `ClientCommandName` più `"log.send_latest"`.
- Tipi `ClientLogFilePayload` (= §2.2) e `ClientLogRecord` (= §4.4).
- `listClientLogs`, `deleteClientLog`. `apiFetch` ora tollera un `204`
  senza corpo.
- `fetchClientLogContent`: la `Response` grezza di `.../content`, solo per la
  route di download.

### 5.2 Download: `app/api/client-logs/[id]/route.ts`

`lib/api.ts` è server-only e la chiave admin non deve arrivare al browser.
Questa route:
- ricontrolla `canUseRemoteControl` (altrimenti `403`);
- valida l'id come ULID (altrimenti `400`);
- fa proxy in streaming di `GET /v2/client/logs/{id}/content`, con
  `?format=gz` facoltativo;
- risponde come allegato con `nosniff` e `no-store`.

### 5.3 `components/remote-control.tsx`

- "Recupera log agent" (`FileText`) tra i comandi di sola lettura, senza
  conferma.
- Sotto `MIN_LOG_UPDATER_VERSION` (`lib/device-status.ts`, **`"1.8.2"`**: va
  aggiornato se la release dell'Updater che lo porta ha un altro numero) il
  pulsante resta visibile ma disabilitato, con un tooltip che dice la versione
  richiesta. `issueCommandAction` ripete lo stesso controllo lato server.
- `disabled_by_policy`: sotto l'errore compare l'indicazione di aggiungere il
  comando a `clientWs.commands`.
- Il `result` di un `log.send_latest` concluso si apre nel viewer invece del
  JSON grezzo.
- Nessuna azione multi-macchina: il comando parte sempre su una macchina sola.

### 5.4 Viewer: `components/client-log-viewer.tsx`

Una sorgente fra due: il `result.payload` ancora nel `CommandRecord`, oppure
un log salvato, letto dalla route di §5.2.

- **Decodifica nel browser**, in `lib/client-logs.ts`: `atob`, poi
  `DecompressionStream("gzip")`. Nessuna libreria.
- **Intestazione**:
  - file, incluso/totale, data di lettura, livello;
  - banner "mostrate le ultime X di Y" se `truncated`;
  - avviso se `mismatch`.
- **Testo**: un `<pre>` con le righe come testo (mai markup), numerate e
  colorate per livello, già scrollato in fondo, dove stanno le righe più
  recenti.
- **Filtri**: toggle per livello e ricerca testuale. Le righe senza un proprio
  `level=` (uno stack trace) ereditano il livello della riga che continuano,
  così un panic e il suo trace restano insieme.
- **Azioni**:
  - "Copia" copia le righe visibili;
  - `.log` è il testo decodificato, scaricato in locale;
  - `.log.gz` è il gzip salvato dall'API, dalla route di §5.2.

### 5.5 Pagina dettaglio macchina: `components/client-logs-card.tsx`

Una card "Log dell'agent" in fondo a `clients/[id]`, visibile solo a chi ha
`canUseRemoteControl` (i log contengono utenti, IP, URL).
- Elenca i log salvati: data, incluso/totale, parziale, versione, chi l'ha
  chiesto.
- Ogni riga si espande nel viewer e si può eliminare, con conferma.
- La card non lancia comandi: il link porta al controllo remoto.

Server action: `listClientLogsAction` e `deleteClientLogAction` in
`lib/actions/client-commands.ts`, con lo stesso `requireAdmin` degli altri.

### 5.6 i18n

`remote.commands.logSendLatest`, `remote.needsVersion`,
`remote.disabledByPolicyHint`, `remote.log.*`, in `it` ed `en`.

## 6. Ordine di rilascio

1. **API**: rilasciabile da sola. Nessun client chiede ancora il comando, e
   la migration 27 crea solo una tabella nuova.
2. **Dashboard**: il pulsante resta disabilitato finché nessuna macchina ha la
   versione minima.
3. **Updater**: release con questo branch. Va fatto il bump di
   `versioninfo.json` più `go generate`, come da checklist di AGENTS.md, e va
   allineato `MIN_LOG_UPDATER_VERSION` della dashboard se il numero non è
   1.8.2.
4. **Documento**: aggiungere `log.send_latest` a `clientWs.commands` **solo
   quando tutta la flotta è sulla release del punto 3**.
   - Un agente ≤ 1.8.1 non conosce il nome e rifiuta *l'intero* documento,
     anche se il comando compare solo nella patch di un override che non lo
     riguarda. Resterebbe sulla revisione in cache per tutto: server, kill
     switch, prodotti. È la stessa trappola di `products` (AGENTS.md §
     *Common Pitfalls*).
   - Se esistono istanze API con `CONFIG_UPSTREAM_URL` (mirror di
     configurazione), devono avere la build del punto 1 prima della
     pubblicazione: `internal/configmirror` valida con lo stesso
     `remoteconfig.Parse`.
   - Per un pilota prima di allora si usa un `run` locale con un documento di
     test (README § *Testing end-to-end without infrastructure*), non un
     override in produzione.

## 7. Fuori scope

- Log ruotati (`updater-*.log.gz`) e il log accanto all'exe. Se servono, un
  `args.file` futuro con un enum chiuso, mai un path libero.
- Log di EMLy (l'app): è un altro processo e un altro utente. Sarebbe un
  comando a parte.
- Chunking su più frame o upload HTTP del file intero. Si rivaluta solo se la
  coda da ~500 KB si rivela insufficiente nella pratica. In quel caso conviene
  un upload paced dal server (vedi la download queue), non un'altra raffica sul
  link MPLS.
- Raccolta automatica (per esempio allegare il log a ogni `update.failed`).
