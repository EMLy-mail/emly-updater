# Coda dei download sull'MPLS — design

Data: 2026-09-28. Repo coinvolti: `emly-go-api` (coda e limite di banda) ed
`emly-updater` (client). Il protocollo WS v2 cambia (nuovo topic), quindi lo
sviluppo va su un branch dedicato in entrambi i repo.

## 0. Contesto

Vedi AGENTS.md § *Deployment topology*. In breve:

- una sola VM ospita `emly-go-api`, MySQL e S3; `baseServer`, `backupServer` e
  `defaultServer` sono IP diversi della stessa macchina;
- circa 350 client su 3 sedi, più i PC da casa, arrivano **tutti** passando
  dall'MPLS, che è l'unico collo di bottiglia;
- quando si pubblica una release di EMLy o dell'updater (setup da 5–10 MB),
  la linea si satura: sono stati misurati circa 200 KB/s per client.

Vincoli dati dall'utente:

- **nessun mirror** e nessuna cache per sede, perché non esistono;
- **niente "scarica prima, installa dopo"**: un aggiornamento urgente deve
  partire subito;
- le macchine con un **utente collegato** (sessione console o RDP) hanno la
  precedenza.

### 0.1 Un problema che oggi aggrava la saturazione

Il download passa già dall'API (`GET /v2/updates/releases/{version}/download`
e `GET /v2/updates/download/updater/{version}`): `streamInstaller` legge da S3
e scrive al client. Però `main.go` applica a **tutte** le route
`chiMiddleware.Timeout(30 * time.Second)`. Il commento in
`internal/updates/download.go` lo dice già: sotto i ~270 KB/s un setup da
8 MB non finisce in 30 secondi.

Sulla linea saturata succede questo:

1. il server interrompe lo stream a 30s, quando il client ha già ricevuto
   6–7 MB;
2. il client riceve un file troncato, e `download.Manager.Ensure` cancella il
   `.partial` (`defer os.Remove(partial)`);
3. al poll successivo il download riparte da zero.

Ogni tentativo fallito spreca quindi quasi un setup intero di banda MPLS, e
più la linea è satura più tentativi falliscono. Nei log dell'API questo
compare come `reason=server timeout` nei warning di `streamInstaller`: va
controllato per primo, perché da solo spiega una parte della saturazione.

## 1. Obiettivi e non-obiettivi

Obiettivi:

- **limitare la banda totale** usata dai download sull'MPLS, lasciando
  sempre margine al resto del traffico (RDP, applicativi);
- **limitare quanti download sono attivi contemporaneamente**, così i primi
  client finiscono in pochi minuti invece di finire tutti insieme alla fine;
- **dare la precedenza** alle macchine con un utente collegato;
- **evitare di scaricare due volte gli stessi byte** (download ripresi da
  dove si erano interrotti);
- partire **subito** quando si pubblica una release: nessuna finestra di
  attesa, nessun gruppo a tempo fisso.

Non-obiettivi:

- ridurre la quantità totale di dati (il tempo per aggiornare tutti dipende
  solo dalla banda della linea, vedi §6);
- gruppi di rilascio per il rischio (un gruppo pilota prima degli altri):
  sono utili ma rispondono a un problema diverso, restano una possibile
  aggiunta futura;
- limiti diversi per sede: tutto passa dalla stessa linea, quindi basta un
  unico gruppo globale.

## 2. Fase 0 — Togliere il timeout di 30s dai download (solo API, subito)

È una modifica piccola e indipendente dalle altre fasi.

- Le due route di download escono dal `chiMiddleware.Timeout(30s)`, montate
  come già si fa con `/v2/client/ws` su un proprio stack di middleware,
  oppure con un gruppo chi senza quel middleware.
- Al suo posto c'è un **timeout di inattività**: lo stream viene chiuso se
  non riesce a scrivere nulla per 60s. Un download lento ma che avanza non
  viene mai interrotto. Un tetto assoluto largo (ad esempio 20 minuti) resta
  solo come protezione.
- I warning di `streamInstaller` restano: con questa modifica
  `reason=server timeout` deve sparire dai log.

## 3. Fase 1 — La coda nell'API

### 3.1 Parametri: env come default, dashboard per cambiarli al volo

I parametri servono solo lato server, quindi **non** vanno nel documento di
configurazione remota, che è validato da entrambi i repo con le fixture
condivise. Hanno due livelli:

1. **Variabili d'ambiente**: sono i default, letti all'avvio in
   `internal/config`.
2. **Override dalla dashboard**: salvato in MySQL e applicato subito, senza
   riavvio. Vince sull'env finché esiste; "Ripristina default" lo cancella e
   si torna al valore dell'env.

| Env | Default | Da dashboard | Significato |
|---|---|---|---|
| `DOWNLOAD_PACING_MODE` | `observe` | sì | `off` / `observe` (non blocca nessuno, logga solo quello che farebbe) / `enforce` |
| `DOWNLOAD_MAX_CONCURRENT` | 5 | sì | posti: download attivi contemporaneamente. `0` = **pausa**, tutti ricevono `429` |
| `DOWNLOAD_MAX_BYTES_PER_SEC` | da definire (§6) | sì | banda totale di tutti i download attivi. `0` = nessun limite di banda (restano i posti) |
| `DOWNLOAD_GRANT_HOLD_SECONDS` | 90 | no | per quanto tempo un posto resta riservato al client avvisato |
| `DOWNLOAD_QUEUE_TTL_SECONDS` | 1200 | no | dopo quanto un client in coda che non si fa più sentire viene tolto |

I due parametri tecnici in fondo restano solo da env: si impostano una volta
e non servono in una situazione d'emergenza.

**Salvataggio.** Una tabella a riga singola, `download_pacing_settings`
(`mode`, `max_concurrent`, `max_bytes_per_sec`, tutti nullable: `NULL`
significa "usa l'env"; più `updated_at` e `updated_by`), con la sua
migrazione in `internal/database/schema/migrations`. Si segue lo schema di
`middleware.BanList`:

- `Reload` viene chiamato subito dalla route admin, così la modifica vale
  all'istante;
- in più c'è un refresh ogni 30s come rete di sicurezza;
- se il DB non risponde, restano in uso gli ultimi valori noti, e all'avvio
  i valori dell'env.

**Route admin:**

- `GET /v2/admin/downloads/pacing`: i valori in uso, con la fonte di ognuno
  (`env` o `dashboard`), più lo stato in tempo reale di posti attivi e coda
  (vedi §3.6);
- `PUT /v2/admin/downloads/pacing`: imposta uno o più override. Validazione:
  `max_concurrent` fra 0 e 50, `max_bytes_per_sec` 0 oppure ≥ 16 KB/s;
- `DELETE /v2/admin/downloads/pacing`: ripristina i valori dell'env.

Ogni modifica viene loggata a livello Info, con il valore precedente e il
nuovo.

**Cosa succede quando cambiano a caldo:**

- **N aumenta**: i nuovi posti vengono assegnati subito ai primi della coda
  (riserva + `download.granted`), senza aspettare che un download finisca;
- **N diminuisce**: nessun download in corso viene interrotto; semplicemente
  non si assegnano nuovi posti finché i download attivi non scendono sotto N;
- **N = 0 (pausa)**: come il caso precedente, i download in corso finiscono
  e nessuno nuovo parte. Il `Retry-After` è fisso a 300s. Serve come freno
  d'emergenza se la linea soffre per altri motivi;
- **la banda cambia**: `rate.Limiter.SetLimit` / `SetBurst` hanno effetto
  sul blocco successivo di ogni stream attivo;
- **modalità da `enforce` a `off`**: la coda si svuota e i client in attesa
  vengono serviti al loro prossimo tentativo.

**Dashboard** (`emly-dashboard-react`): una pagina "Distribuzione
aggiornamenti" con:

- i tre valori modificabili, con accanto la fonte (env o dashboard) e il
  pulsante "Ripristina default";
- un pulsante "Pausa", che imposta N = 0;
- in tempo reale, i posti occupati (host, avanzamento, velocità) e la coda
  (host, priorità, attesa).

### 3.2 Limite di banda

Un unico `rate.Limiter` (`golang.org/x/time/rate`) condiviso da tutti gli
stream attivi. `streamInstaller` scrive a blocchi da 32 KB e, prima di ogni
blocco, chiama `WaitN(ctx, len)`. La banda totale non supera mai
`maxBytesPerSec` e si divide in modo naturale fra i download attivi.

### 3.3 Posti e coda

Stato in memoria nell'API, non in MySQL (vedi §3.6):

- `active`: i download in corso, identificati dall'HWID (o dall'IP se
  l'header manca);
- `queue`: i client in attesa, ordinati per priorità e poi per ora di
  arrivo.

Priorità, letta dagli header che l'updater manda già a ogni richiesta
(`updaterclient.IdentityFromRequest`):

1. `X-EMLy-LoggedUserState` = `active-console` o `active-rdp`;
2. `disconnected`;
3. nessun utente collegato.

Quando arriva una richiesta di download:

```
se mode == off                        → servi il file
se il client è già in active          → servi (è la ripresa di un download)
se active < maxConcurrent e
   (il client ha il posto riservato,
    oppure nessun posto è riservato
    e la coda è vuota o lui è il primo) → aggiungilo ad active e servi
altrimenti                            → mettilo in coda (o aggiornane l'ora),
                                        rispondi 429 + Retry-After
```

Quando uno stream si chiude (finito, interrotto o timeout), il client esce
da `active`. Se c'è qualcuno in coda:

- il posto viene **riservato** al primo della coda per `grantHoldSeconds`;
- se quel client è connesso al canale WS e dichiara il topic
  `download.granted`, riceve il notify subito (§4.3);
- se non si presenta entro `grantHoldSeconds`, la riserva passa al
  successivo.

`Retry-After` si stima dalla posizione in coda, dal numero di posti e dalla
durata media degli ultimi download. Va limitato fra 30s e 900s (il poll di
default è di 15 minuti).

### 3.4 I client che non conoscono ancora la coda

Gli updater già installati trattano il `429` come un download fallito e
riprovano al poll successivo. Non aspettano il `Retry-After`, quindi non
possono tenere un posto in coda. Per non escluderli:

- un client che **non** dichiara `download.granted` nelle capabilities del
  canale WS, e non ha una connessione WS attiva, **viene servito se al suo
  arrivo c'è un posto libero e non riservato**, indipendentemente dalla coda;
- altrimenti riceve `429` e riproverà al poll successivo.

Così il primo rilascio dell'updater che contiene la coda arriva anche alle
macchine vecchie: più lentamente, ma senza saturare la linea. Per questi
client la priorità vale solo in parte.

### 3.5 Richieste parziali (`Range`)

Per la ripresa lato client (§4.2) l'API deve supportare le richieste
parziali:

- `S3Connector` ottiene un `GetFileRange(ctx, key, from)`, che passa `Range`
  alla `GetObject` di S3;
- la risposta ha `Accept-Ranges: bytes` ed `ETag`; con un `Range` valido
  risponde `206` con `Content-Range`;
- se `If-Range` non corrisponde all'`ETag` attuale (la release è stata
  ricaricata), risponde `200` con il file intero.

Un download ripreso entra nella coda come uno nuovo. Il client che era già
in `active` rientra subito, perché la ripresa arriva immediatamente dopo
l'interruzione.

### 3.6 Stato e osservabilità

- La coda è **in memoria**: se l'API si riavvia, la coda si perde e i client
  rientrano al tentativo successivo. Non serve scrivere lo stato della coda
  su MySQL, che sta sulla stessa VM.
- `GET /v2/admin/downloads/pacing` (§3.1) restituisce, oltre ai parametri:
  posti attivi (host, byte inviati e totali, velocità), coda (host, priorità,
  attesa) e il numero di byte al secondo effettivamente usati.
- Il log `observe` riporta, per ogni richiesta che avrebbe ricevuto un `429`,
  host, priorità e posizione in coda: permette di tarare i parametri prima di
  passare a `enforce`.

## 4. Fase 2 — L'updater

### 4.1 Il `429` è una coda, non un errore

- `source.HTTPSource.FetchSetup` riconosce `429` e restituisce
  `*source.QueuedError{RetryAfter}`.
- `download.Manager.Ensure` lo propaga senza cancellare il `.partial`.
- In `service.go` (download di EMLy) e in `selfupdate.go` (download
  dell'updater), un `QueuedError`:
  - **non** emette `update.failed` né `download_failed`, e **non** conta come
    tentativo;
  - logga a livello Info solo il primo accodamento per versione, poi a Debug;
  - chiama `scheduleWake("queued", retryAfter)` limitato fra 30s e
    l'intervallo di poll, **senza** passare da `notifyWakeThrottle`, che
    serve per i notify e non per questo caso.

### 4.2 Ripresa del download

- Il `.partial` non viene più cancellato quando il download si interrompe o
  va in coda: viene cancellato solo se la verifica SHA256 finale fallisce.
- Se esiste un `.partial`, `FetchSetup` manda
  `Range: bytes=<dimensione>-` e `If-Range: <ETag salvato>`. L'ETag si salva
  in un file accanto, `<partial>.etag`.
  - `206`: scrive in coda al file;
  - `200`: ricomincia da zero, perché il file sul server è cambiato;
  - `416`: il file era già completo, si passa alla verifica.
- Nessuna modifica alla verifica: SHA256 sul file intero, come oggi.
- `CleanupExcept` cancella già i `.partial` delle altre versioni, quindi non
  serve altro.

### 4.3 Nuovo topic `download.granted`

- `wsclient.TopicDownloadGranted = "download.granted"`, con payload
  `{target, version}`. Va aggiunto a `TopicNames`, così compare nelle
  capabilities.
- `handleNotify`: se target e versione corrispondono a un download in coda,
  `scheduleWake("granted", 5s)`, senza passare da `notifyWakeThrottle`.
- Il topic va aggiunto anche a `clientproto` in `emly-go-api`: il protocollo
  WS è duplicato fra i due repo come `proto/updateripc.proto`, e nulla ne
  verifica l'allineamento.

### 4.4 Timeout HTTP

`NewHTTPSource` usa `http.Client{Timeout: 10 * time.Minute}` per tutta la
richiesta. Con la ripresa, un timeout non fa più perdere dati, ma va
comunque rispettato: con un setup da 10 MB, la velocità per client
(`maxBytesPerSec / maxConcurrent`) non deve scendere sotto circa 17 KB/s
(vedi §6).

## 5. Ordine di rilascio

1. **API, fase 0** (timeout di 30s): subito, da sola.
2. **API, fase 1** in `observe`: raccoglie dati reali per tarare i
   parametri, senza cambiare il comportamento.
3. **API in `enforce`**, con `maxConcurrent` e `maxBytesPerSec` scelti dai
   log. Da qui la linea è protetta anche per i client vecchi (§3.4).
4. **Updater con la fase 2** (nuova versione, nuovo `ProtocolVersion` nella
   matrice): si distribuisce passando dalla coda stessa.
5. Facoltativo, dopo: l'API manda `release.published` in automatico quando
   si pubblica una release. Con la coda attiva è sicuro, perché i client in
   eccesso vanno semplicemente in coda.

## 6. Dimensionamento

Con `S` la dimensione del setup, `C` il numero di client e `R` la banda
concessa ai download (`maxBytesPerSec`):

- tempo per aggiornare tutti ≈ `C × S / R`;
- velocità per client = `R / maxConcurrent`;
- tempo per un client ≈ `S × maxConcurrent / R`.

**Misura reale (iperf3, 2026-09-28):** circa 42 Mbit/s (≈ 5,2 MB/s) stabili,
da `172.16.96.65` a `172.16.33.72`, un solo stream, test di 25 secondi.

Valori proposti come default in env: `R` = 3 MB/s (circa 25 Mbit/s, il 60%
della linea, lasciando circa 17 Mbit/s al resto del traffico) e
`maxConcurrent` = 5, con `S` = 8 MB e `C` = 350:

- velocità per client circa 600 KB/s, quindi un setup in circa 13 secondi;
- 25 macchine con utente collegato aggiornate in poco più di un minuto;
- tutta la flotta aggiornata in circa 15–16 minuti.

Un aggiornamento urgente arriva quindi a tutti in circa un quarto d'ora,
senza mai occupare più del 60% della linea.

Da questi numeri segue una cosa: a 42 Mbit/s l'intera flotta si
aggiornerebbe in circa 9 minuti anche oggi, se i download andassero a buon
fine. Una saturazione che dura molto di più indica che molti download
vengono troncati e ripetuti (§0.1): la fase 0 è quindi probabilmente la
correzione con l'effetto maggiore.

Da verificare prima di fissare `R`:

- **Direzione:** iperf3 senza `-R` misura dal client al server. I download
  vanno nella direzione opposta, dalla VM ai client: serve
  `iperf3 -c <VM> -R` da un PC in una sede remota, perché le linee MPLS
  possono essere asimmetriche.
- **Quale tratta:** va chiarito se i 42 Mbit/s sono la porta MPLS della sede
  del server (condivisa da tutte le sedi e dai PC da casa) o il circuito di
  una sola sede remota. Il limite `R` va sulla tratta più stretta fra quelle
  che tutti condividono.
- **Orario:** va ripetuta in orario di lavoro, con il traffico normale in
  corso, e con `-P 5` per simulare 5 download in parallelo.

## 7. Domande aperte

- Conferma della misura nella direzione giusta e sulla tratta condivisa (§6).
- I PC da casa hanno sempre `X-EMLy-LoggedUserState` valorizzato? In ogni
  caso rientrano nella stessa coda.
