# Agent multi-prodotto — design

Data: 2026-09-30. Repo coinvolti: `emly-updater` (motore, driver, policy) ed
`emly-go-api` (validatore della sezione `products` del documento di remote
config). Branch: `feat/multi-product` in entrambi.

Controparte API: la spec "API multi-prodotto — spec per l'Agent" del
2026-09-30 (team `emly-go-api`), che ha introdotto `/v2/updates/{slug}/…`, il
registro prodotti e l'inventario `X-EMLy-InstalledProducts` / `installed_products`
(quest'ultimo già implementato su questo branch).

## 0. Contesto

Oggi l'Agent sa aggiornare un solo prodotto, EMLy, e lo sa in circa quindici
punti sparsi in `internal/service/service.go`: `ResolveEMLy` (`GUI_SEMVER`),
`EMLyExeName`, `installer.Run` con `/FORCEUPGRADE`, `VerifyInstalled` sul
`config.ini` di EMLy, `assoc.Repair`, i testi della notifica, `Target: "emly"`
negli eventi WS, lo slot unico `state.Pending`.

Esiste già un secondo prodotto da gestire, **3g-RocketChat**:

| | |
|---|---|
| slug API | `3g-rocketchat` (`/v2/updates/3g-rocketchat/manifest`) |
| directory | `C:\3gIT\3g-RocketChat` (`Users:(RX)`, non scrivibile dagli utenti) |
| exe | `3g-RocketChat.exe` — processo utente, va atteso/chiuso prima dell'install |
| installer | NSIS 3.12, `requireAdministrator`; in futuro forse Inno Setup |
| uninstaller | `uninstall.exe` (NSIS); chiave `HKLM\…\Uninstall\3gIT3g-RocketChat`, senza `DisplayVersion` |
| versione | `version.txt` (scritto dal setup a ogni install) |

Due fatti verificati sulla macchina di sviluppo condizionano il rilevamento:

- `config.ini` → `[app] version` **non è affidabile oggi**: il file dichiara
  "Il setup crea questo file solo se non esiste: gli aggiornamenti non lo
  toccano", quindi dopo il primo update resterebbe fermo alla versione
  iniziale.
- `3g-RocketChat.exe` **non ha VERSIONINFO** (`FileVersion`/`ProductVersion`
  vuoti).

La direzione voluta è passare a `config.ini`: il setup di RocketChat dovrà
aggiornare `[app] version` a ogni install. Fino ad allora la fonte è
`version.txt`.

Sull'API beta (`emly-cb:8081`) il manifest di RocketChat risponde `200` con
`stableVersion: ""` (prodotto registrato, nessuna release pubblicata) e
`/v2/config` (revision 15) non ha ancora una sezione `products`. Il registro
prodotti dell'API (`internal/productreg`) conosce solo `slug`, `name`,
`s3_prefix`, `enabled`: non sa come si rileva né come si installa un
prodotto, e `/v2/products` è solo admin. Per questo le definizioni vivono
nel documento di remote config, non in una API prodotti separata.

## 1. Obiettivi e non-obiettivi

Obiettivi:

1. Un unico motore di aggiornamento (manifest → download → verifica →
   install → verifica) che serve EMLy e qualunque prodotto definito nel
   documento di remote config.
2. EMLy migra sul motore **senza cambi di comportamento**.
3. 3g-RocketChat aggiornabile tramite setup NSIS, con rilevamento
   `version.txt` → `config.ini` → VERSIONINFO.
4. Inventario `X-EMLy-InstalledProducts` esteso a tutti i prodotti.

Non-obiettivi di questa release:

- eventi WS `update.*` e comandi WS per prodotti diversi da EMLy (il campo
  `target` ammette solo `emly|updater`; serve un cambio di protocollo con
  capability — spec API §3.2–3.3);
- IPC, `control.app` e `emly.manifest.check` restano specifici di EMLy;
- comandi di install/rilevamento liberi da configurazione.

## 2. Scelta architetturale

Scelto **un motore generico su cui migra anche EMLy** (approccio A), contro
un secondo ciclo parallelo per i soli prodotti nuovi (B) e una goroutine per
prodotto (C).

- B duplicherebbe `Cycle`/`apply`/`install`/`forceRedownload` (~400 righe),
  cioè proprio il codice su cui sono caduti i fix recenti (gate distruttivo
  ricontrollato dopo il countdown, coda download piena, reinstall pulito):
  ogni fix futuro andrebbe fatto due volte.
- C scaricherebbe e installerebbe in parallelo: più traffico concorrente
  sull'MPLS (vedi AGENTS.md § *Deployment topology*) e due setup in
  competizione per `beginInstall`.

Il rischio di A (rompere EMLy) si copre con la fase 1 (§9): estrazione del
motore con il solo EMLy, suite esistente verde senza toccare i test.

## 3. Componenti

### 3.1 `internal/product` (nuovo)

Cos'è un prodotto, senza I/O di rete.

```go
type Product struct {
    Slug         string          // "emly", "3g-rocketchat"
    Name         string          // testi utente e log
    ManifestPath string          // emly: "/v2/updates/manifest"; altri: "/v2/updates/{slug}/manifest"
    InstallDir   string          // assoluto
    ExeName      string          // processo da attendere/chiudere; icona della progress window
    Detect       []VersionSource // catena: la prima fonte che risponde vince
    Installer    InstallerSpec   // driver + opzioni chiuse
    Channel      string          // "stable" | "beta"
    InstallWhenAbsent bool
    Legacy       bool            // solo emly: abilita gli extra di §3.4
}
```

`VersionSource` è un tipo chiuso, implementato nel codice:

| tipo | parametri | lettura |
|---|---|---|
| `ini` | `path`, `section`, `key` | valore della chiave, spazi rimossi |
| `file` | `path` | contenuto intero, spazi e BOM rimossi, prima riga |
| `exe` | `path` | VERSIONINFO `ProductVersion`, poi `FileVersion` (`GetFileVersionInfoW`) |

I `path` sono relativi a `InstallDir`. Per EMLy la catena è un solo `ini` sul
`config.ini` configurato localmente (`EMLyConfigFile`, sezione `EMLy`, chiave
`GUI_SEMVER`), identico a oggi.

`Detect` restituisce uno di tre esiti:

- `installed(v)`: una fonte esiste e dà una versione parsabile;
- `absent`: **nessuna** fonte della catena esiste (file mancanti);
- `unknown`: una fonte esiste ma è illeggibile, non ha la chiave o dà una
  versione non parsabile, e nessuna fonte precedente ha risposto.

Regola della catena: si scorre in ordine; una fonte il cui file non esiste
passa alla successiva; la prima che dà `installed` vince; se una fonte
esiste ma è rotta e nessuna successiva dà `installed`, l'esito è `unknown`
(non `absent`). `unknown` non produce mai un install.

### 3.2 `internal/installer` (a driver)

```go
type Driver interface {
    Install(setupPath, version string) error
    Uninstall() error // nil se non c'è uninstaller
}
```

La verifica post-install non è del driver: si riesegue `Detect` del prodotto
e si richiede `installed(v)` con `v >= target` (stessa regola di
`VerifyInstalled` oggi).

**`inno`** — il codice di oggi: `/VERYSILENT /SUPPRESSMSGBOXES /NORESTART
/LOG=<logs>\<slug>-install-<version>.log`, più `/FORCEUPGRADE` solo se
`Legacy`. Uninstall: `unins*.exe` in `InstallDir`, stessi argomenti con il
proprio `/LOG`. Per EMLy il nome del log resta `emly-install-<version>.log`.

**`nsis`**:

- setup: `setup.exe /S /D=<InstallDir>`. `/D=` è sempre **l'ultimo**
  argomento e **senza virgolette**, anche con spazi (regola di NSIS). Si
  passa sempre, così il setup installa esattamente dove `Detect` guarda;
- uninstall: `<InstallDir>\uninstall.exe /S _?=<InstallDir>`. Senza `_?=`
  l'uninstaller NSIS si copia in `%TEMP%`, si rilancia e ritorna subito,
  quindi l'Agent partirebbe col reinstall a disinstallazione in corso;
- exit code: `0` = ok, altro = errore. NSIS non ha `/LOG`: si logga l'exit
  code. NSIS può uscire con `0` anche su fallimenti parziali: decide la
  verifica.

Timeout 15 minuti per entrambi (`runSilent` condiviso, invariato).

**Controllo ACL prima dell'uninstaller** (per tutti i prodotti, EMLy
compreso): l'uninstaller gira come SYSTEM ed è preso da `InstallDir`, che
per i prodotti generici arriva da configurazione. Prima di eseguirlo si
controlla che né `InstallDir` né l'eseguibile diano diritti di scrittura a
`Users`, `Authenticated Users` o `Everyone`; altrimenti il passo di uninstall
viene saltato con un Warn e il ritentativo prosegue senza.

### 3.3 Motore in `internal/service`

`Cycle`, `apply`, `install`, `forceRedownload`, `runSetupAndVerify` ricevono
un `*product.Product` invece di leggere `u.Cfg.EMLy*`. Il self-update
dell'Agent non è un prodotto e non cambia.

Un `download.Manager` per prodotto, tutti nella stessa `downloads\`, con
`Prefix` diverso (EMLy tiene `EMLy-`, gli altri `<slug>-`) e **un unico
`Pacer` condiviso** con il manager dell'updater: la coda del server è una
sola, un `429` su un prodotto trattiene anche gli altri. `CleanupExcept`
tocca già solo il proprio prefisso.

### 3.4 Extra riservati a EMLy (`Legacy`)

- `assoc.Repair` dopo l'install;
- avviso critico e notifica "app aperta" localizzati con `LANGUAGE` di EMLy
  (gli altri prodotti: italiano, come la progress window);
- canale da `config.ini` di EMLy, con `updater.channelOverride` che vince;
- fresh install quando EMLy è assente (comportamento attuale);
- `WaitForExit` bloccante quando l'app è aperta e l'update non è forzato
  (§5.2);
- ritentativo pulito sempre con uninstall, nessun tetto ai tentativi (§6);
- eventi WS `update.*` con `target: "emly"`;
- manifest sul percorso storico `/v2/updates/manifest` (vale anche per i
  mirror non aggiornati).

## 4. Sezione `products` del documento di remote config

```json
"products": {
  "3g-rocketchat": {
    "enabled": true,
    "name": "3g-RocketChat",
    "channel": "stable",
    "installDir": "C:\\3gIT\\3g-RocketChat",
    "exeName": "3g-RocketChat.exe",
    "installWhenAbsent": false,
    "detect": [
      { "type": "file", "path": "version.txt" },
      { "type": "ini",  "path": "config.ini", "section": "app", "key": "version" },
      { "type": "exe",  "path": "3g-RocketChat.exe" }
    ],
    "installer": { "type": "nsis", "cleanReinstall": false }
  }
}
```

Regole di validazione (identiche in Agent e API, con fixture condivise):

| campo | regola | default |
|---|---|---|
| chiave | `^[a-z0-9][a-z0-9-]{0,19}$`; non riservata (`updater`, `all`, `manifest`, `releases`, `download`, `products`); **non `emly`** | — |
| `enabled` | bool | `false` |
| `name` | non vuoto, max 64 caratteri | — (obbligatorio) |
| `channel` | `stable` \| `beta` | `stable` |
| `installDir` | percorso Windows assoluto (`X:\…`), niente `..` | — (obbligatorio) |
| `exeName` | nome file senza separatori, termina in `.exe` | — (obbligatorio) |
| `installWhenAbsent` | bool | `false` |
| `detect` | 1–5 elementi; `type` ∈ `ini`/`file`/`exe`; `path` relativo, senza `..`, senza `:` (né `C:foo` relativo al drive né stream NTFS `file:x`), non assoluto; `ini` richiede `section` e `key` | — (obbligatorio) |
| `installer.type` | `nsis` \| `inno` | — (obbligatorio) |
| `installer.cleanReinstall` | bool | `false` |

Motivazioni:

- **`emly` vietato**: EMLy resta configurato dove sta oggi
  (`updater.channelOverride`, `config.ini` locale) ed è il prodotto
  implicito. Un documento senza `products` si comporta esattamente come
  oggi.
- **Oggetto indicizzato per slug, non array**: gli override sono
  merge-patch, quindi un override può spegnere un solo prodotto
  (`{"products": {"3g-rocketchat": {"enabled": false}}}`) senza riscrivere
  la lista.
- **`enabled` spegne gli update, non il rilevamento** (§7).
- **`installWhenAbsent: false`**: si aggiorna solo dove il prodotto è già
  installato; per distribuirlo su un sito si attiva con un override.
- **Nessun argomento libero**: il documento sceglie tra comportamenti
  implementati nel codice, non può far eseguire comandi arbitrari come
  SYSTEM.

`products` entra in `PatchableSections` e tra le sezioni del documento
effettivo. `schemaVersion` resta `1`.

### 4.1 Compatibilità con gli Agent già in campo

- Un `products` **globale** è sicuro: `policy.complete` copia solo le sezioni
  che conosce, un Agent ≤ 1.7.x lo ignora.
- Un **override** che patcha `products` fa **rifiutare l'intero documento**
  agli Agent ≤ 1.7.x (`parse.go`: "not a patchable section"), che restano
  sulla revisione in cache per *tutto* (server, kill switch, …). Stesso
  caso di `clientWs` a settembre.

Regola operativa: **niente override su `products` finché la flotta non è
tutta ≥ 1.8.0**. Va in AGENTS.md § *Common Pitfalls*. Lato API, bloccare il
salvataggio di un tale override finché `updater_clients` ha Agent più
vecchi è desiderabile ma fuori da questa spec.

## 5. Flusso del ciclo

```
Cycle:
  certificato → control gate → gate distruttivo → self-update Agent   (invariati)
  lista = prodotti del documento con enabled=true, ordinati per slug, poi emly
  per ogni p, in sequenza:
      gate distruttivo → se pendente: stop del giro
      Detect(p):
          unknown                          → Warn una volta per sessione, salta
          absent && !InstallWhenAbsent     → salta, nessun poll del manifest
      p non disponibile (§5.3) e non scaduto → salta
      pending[p]:
          ancora necessario e sha256 ok    → apply(p)
          soddisfatto                      → ClearPending, cleanup, continua col poll
          corrotto                         → rimuovi, ClearPending, continua col poll
      poll manifest (catena server, ManifestPath di p)
          404 / release vuota              → non disponibile (§5.3)
          target <= installata             → cleanup, fine per p
          altrimenti                       → download (Pacer) → SetPending → apply(p)
      errore su p → log, si passa al successivo
```

EMLy è **ultimo** perché può bloccare (§5.2): così non ritarda mai gli altri
prodotti.

### 5.1 Un solo download e un solo setup alla volta

Il giro è sequenziale nella goroutine di poll: mai due download o due setup
contemporanei sulla stessa macchina. `beginInstall("<slug> install")`
resta il checkpoint autorevole contro un comando distruttivo ammesso a metà
giro.

### 5.2 App aperta

- **Update forzato** (`isCritical`, o installata < `minRequiredVersion`): come
  EMLy oggi — avviso con countdown (`updater.criticalWarning`), gate
  distruttivo ricontrollato, `process.TerminateAll(ExeName)`, install.
- **Non forzato, prodotto generico**: **non blocca**. Il pending resta, parte
  una notifica per la sessione utente una volta per versione (set in memoria;
  dopo un riavvio del servizio può ripartire), e il giro prosegue. Una
  goroutine per prodotto (`watchProductExit`) attende l'uscita dell'app
  (`WaitForExitUnder`) e sveglia subito il loop: il ciclo svegliato installa.
  La goroutine non installa mai da sé, così resta un solo setup alla volta
  (§5.1). Se in quel momento EMLy blocca il ciclo in `WaitForExit`, il
  risveglio aspetta che EMLy finisca.
- **Non forzato, EMLy**: `WaitForExit` bloccante, come oggi.

Testo della notifica per i prodotti generici (solo italiano):

> **<Name> - Aggiornamento in attesa**
> Un aggiornamento di <Name> è pronto. Chiudere l'applicazione per completarlo.

### 5.3 Prodotto non disponibile

`404` sul manifest, oppure `200` con release vuota (`stableVersion` o
`stableDownload` vuoti; per `beta`, stessa regola sul canale beta dopo il
fallback a stable di `ChannelVersion`), significano "nessun aggiornamento
per questo prodotto", non errore:

- se il `404` arriva dal `baseServer` del sito, si prova il `defaultServer`
  prima di concludere (mirror non ancora aggiornato, spec API §2.1);
- il prodotto è marcato non disponibile **per 6 ore**, in memoria;
- un solo log Info per marcatura, niente Warn a ogni ciclo.

Per EMLy non cambia nulla: il suo manifest storico non restituisce mai una
release vuota, e un errore resta un errore.

## 6. Stato, tentativi, errori

### 6.1 `state.json`

```json
{
  "pending":  { "...": "EMLy, formato e campo invariati" },
  "products": {
    "3g-rocketchat": {
      "version": "1.1.0", "setupPath": "…", "sha256": "…",
      "forced": false, "downloadedAt": "…",
      "attempts": 1, "gaveUp": false
    }
  },
  "selfUpdate": "…", "pendingCommands": "…"
}
```

- API per slug: `Store.Pending(slug)`, `SetPending(slug, p)`,
  `ClearPending(slug)`. In memoria una mappa; in serializzazione `emly` va
  nel campo storico `pending`, gli altri in `products`.
- Motivo: un rollback dell'Agent a una versione precedente rilegge il suo
  `pending` e non perde un install di EMLy già scaricato. Un Agent vecchio
  ignora `products`.
- `Pending` di un prodotto generico ha in più `attempts` e `gaveUp`; per
  EMLy restano assenti (omitempty), il file di EMLy è identico a oggi.

### 6.2 Tetto ai tentativi (solo prodotti generici)

Un install che fallisce la verifica (anche dopo il secondo tentativo) conta
un tentativo per la versione target. Al **terzo** fallimento: `gaveUp: true`,
un log Error una volta, e il prodotto non viene più toccato finché il
manifest non offre una **versione diversa** (che azzera il contatore). Stesso
schema di `state.SelfUpdate`. EMLy mantiene il ritentativo a ogni ciclo.

### 6.3 Secondo tentativo

Dopo un primo install la cui verifica fallisce:

- `cleanReinstall: false` (default per i prodotti generici): cache svuotata
  per quel prodotto, nuovo download, setup rilanciato **senza uninstall**.
  Copre file in cache corrotto o build sbagliata senza toccare i dati.
- `cleanReinstall: true`, ed EMLy sempre: percorso attuale di EMLy
  (nuovo download, uninstall — soggetto al controllo ACL di §3.2 — e setup).

Motivo del default: l'uninstaller NSIS tipicamente rimuove `$INSTDIR`; per
RocketChat significherebbe perdere `config.ini` (gestito dall'IT e ricreato
dal setup ai default) e `data\` (dati utente).

### 6.4 Tabella degli esiti

| caso | esito |
|---|---|
| `Detect` unknown | salto del prodotto, Warn una volta; header inventario omesso (§7) |
| manifest `404` / release vuota | non disponibile 6 h (§5.3) |
| coda download piena (`429`) | come EMLy: Info, ritenta al ciclo dopo, `Pacer` condiviso |
| sha256 diverso prima di eseguire | file rimosso, pending rimosso, nuovo download al ciclo dopo |
| setup ok ma verifica fallita | secondo tentativo (§6.3), poi conta per il tetto (§6.2) |
| installata > manifest | nessuna azione (niente downgrade) |
| comando distruttivo a metà giro | stop dei prodotti rimanenti |
| errore su un prodotto | log, il giro prosegue |

## 7. Inventario

`installedProducts` (`service.go`) esegue `Detect` su EMLy e su **tutti** i
prodotti del documento, anche con `enabled: false`.

- mappa con gli `installed(v)`; gli `absent` non compaiono;
- se **anche un solo** prodotto è `unknown`, la mappa è `nil` e
  `X-EMLy-InstalledProducts` / `installed_products` non partono: l'inventario
  è per definizione completo, e ometterne uno lo farebbe risultare
  disinstallato;
- limite noto: togliere un prodotto dal documento lo fa sparire
  dall'inventario, quindi dalla dashboard. Per spegnere gli update si usa
  `enabled: false`. Va in AGENTS.md.

`X-EMLy-AppVersion` continua a riportare EMLy come oggi.

## 8. Interfaccia utente

Progress window e toast: stesso codice, parametrizzato su `Name` e sull'icona
di `InstallDir\ExeName`. Sottotitolo: "<Name> resta utilizzabile" /
"<Name> non è disponibile fino al termine". Testi per EMLy invariati.

## 9. Fasi

**Fase 1 — motore con solo EMLy, nessun cambio di comportamento.**

- `internal/product` con `Product`, `VersionSource` (`ini`), `Detect` a tre
  esiti;
- `internal/installer` a driver, solo `inno`;
- motore parametrizzato su `*product.Product`;
- `state.Store` con API per slug, `emly` serializzato in `pending`;
- criterio d'uscita: la suite esistente di `internal/service` passa senza
  modifiche ai test (solo firme interne se inevitabile), più un test che fa un
  giro di `state.json` e verifica il formato identico a oggi.

**Fase 2 — prodotti generici.**

- sorgenti `file` ed `exe`;
- driver `nsis`, `cleanReinstall`, controllo ACL prima dell'uninstaller;
- sezione `products` in `internal/policy` (decode, validazione,
  `PatchableSections`) e fixture;
- giro sequenziale con EMLy ultimo, non bloccante per i generici;
- non disponibile / `defaultServer`, tetto ai tentativi;
- inventario esteso;
- sottocomando diagnostico (§10).

## 10. Sottocomando `emly-updater products`

Carica la policy effettiva (documento in cache o `config.ini`) e, per ogni
prodotto (EMLy compreso), stampa: slug, `enabled`, esito di `Detect` con la
fonte che ha risposto, pending e tentativi. Con `--check` interroga anche il
manifest sulla catena server e mostra la versione offerta, **senza scaricare
né installare**. Serve a validare la definizione di un prodotto nuovo su una
macchina prima di abilitarlo. Non richiede il servizio fermo (sola lettura;
nessun mutex singleton).

## 11. Test

Go puro, niente admin, come la suite attuale.

- `product`: catena (prima vince, fallback su file mancante, rotta →
  unknown, tutte mancanti → absent), path traversal rifiutato, BOM in
  `version.txt`.
- `installer`: argomenti esatti di `inno`/`nsis` (exe finto che registra i
  propri argomenti), `/D=` ultimo, `_?=` sull'uninstall, controllo ACL.
- `policy`: fixture in `testdata/remoteconfig/valid|invalid` — `emly` in
  `products`, slug riservato, `installDir` relativo, `path` assoluto o con
  `..`, `type` sconosciuto, override su `products`, prodotto disabilitato da
  override.
- `state`: giro completo con `emly` in `pending` e altri in `products`;
  formato EMLy invariato.
- `service`: due prodotti con RocketChat aperto che non blocca EMLy; EMLy
  ultimo; `404` e release vuota in non disponibile; tentativo sul
  `defaultServer`; tetto tentativi e reset su versione nuova;
  `cleanReinstall=false` senza uninstall; gate distruttivo a metà giro;
  inventario `nil` con un unknown.
- `Live` opzionale (`EMLY_EXE_VERSION_TEST_FILE`) per VERSIONINFO su un exe
  reale.

## 12. Lato API (`emly-go-api`, `feat/multi-product`)

- validatore di `products` in `internal/remoteconfig` con le regole di §4;
- `products` tra le sezioni patchabili;
- stesse fixture di `testdata/remoteconfig/` (copia identica, CLAUDE.md §
  *Two things that silently desync*).

Senza questo la dashboard non può salvare un documento con `products`.

## 13. Rilascio

- Nessun cambio IPC o WS: niente bump della matrice di compatibilità.
  Versione **1.8.0** via `versioninfo.json` + `go generate`.
- Ordine di attivazione:
  1. Agent 1.8.0 su tutta la flotta;
  2. `products.3g-rocketchat` globale, `enabled: true`,
     `installWhenAbsent: false`;
  3. prima release di RocketChat pubblicata sull'API;
  4. solo dopo, override per sito (§4.1).
- Documentazione: AGENTS.md (convenzioni prodotti, pitfall override,
  "togliere un prodotto = disinstallato in dashboard", NSIS `_?=` e `/D`
  ultimo, controllo ACL), README (riferimento `products` e ricetta E2E),
  `docs/remote-config.example.json`.

## 14. Fuori da questo repo

- Setup di RocketChat: deve aggiornare `[app] version` in `config.ini` a ogni
  install (solo quella chiave). Solo dopo può smettere di scrivere
  `version.txt`, e la catena scende da sola su `ini`.
- Build di RocketChat: aggiungere VERSIONINFO all'exe e `DisplayVersion` alla
  chiave Uninstall, così il fallback `exe` funziona davvero.
- Protocollo WS per prodotti generici (`product.manifest.check`, `target`
  libero negli `update.*`): spec API §3.2–3.3, da decidere.
