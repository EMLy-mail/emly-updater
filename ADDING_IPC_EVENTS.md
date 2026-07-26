# Come aggiungere un nuovo evento IPC (EMLyUpdater ⇄ EMLy)

Canale: named pipe, protobuf, un `Envelope` request in / un `Envelope` response
out per connessione. Schema in `proto/updateripc.proto`, **sincronizzato a
mano** tra `emly-updater/proto/` e `emly/proto/` (nessun modulo Go condiviso
tra i due repo — un edit da un solo lato desincronizza il wire protocol senza
che nulla lo segnali).

Repo coinvolti:
- `emly-updater` (questo repo) — server, gira come LocalSystem, espone
  `internal/ipc`.
- `emly` — client, apre la connessione verso la pipe.

---

## Caso A — Nuovo tipo di richiesta/risposta (es. `FooRequest`/`FooResponse`)

### 1. Proto (entrambi i repo, file identico)

In `proto/updateripc.proto`:

```proto
message FooRequest {}

message FooResponse {
  string bar = 1;
}
```

Aggiungi i due campi al `oneof body` di `Envelope`, con numeri di campo
**non riusati** (prossimi liberi dopo `99` sono `14`, `15`, ... — tieni
`error = 99` come ultimo):

```proto
oneof body {
  SystemInfoRequest system_info_request = 10;
  SystemInfoResponse system_info_response = 11;
  ADStatusRequest ad_status_request = 12;
  ADStatusResponse ad_status_response = 13;
  FooRequest foo_request = 14;
  FooResponse foo_response = 15;
  ErrorResponse error = 99;
}
```

Copia il file **verbatim** nell'altro repo. Nessun automatismo lo forza:
desync silenzioso se salti questo passo.

### 2. Rigenera `ipcpb` (entrambi i repo)

```powershell
go generate ./internal/ipc/ipcpb
# oppure a mano:
protoc --go_out=. --go_opt=paths=source_relative -I proto proto/updateripc.proto
```

Richiede `protoc` + `protoc-gen-go` installati. Non serve per build/CI normale
(il file generato è committato), ma serve appena tocchi il `.proto`.

### 3. Lato server — `emly-updater` (`internal/ipc/server.go`)

Aggiungi un case in `dispatch()`:

```go
case *ipcpb.Envelope_FooRequest:
    // logica per produrre la risposta
    return &ipcpb.Envelope{
        ProtocolVersion: ProtocolVersion,
        SenderVersion:   version.Version,
        Body: &ipcpb.Envelope_FooResponse{
            FooResponse: &ipcpb.FooResponse{Bar: "..."},
        },
    }
```

Se serve un dato non ancora raccolto, valuta se aggiungerlo a
`machineinfo.Info` (raccolto una volta all'avvio del servizio, non per ogni
richiesta IPC — vedi commento su `machine func() machineinfo.Info` in
`server.go`) o se va calcolato al momento nella dispatch.

### 4. Lato client — `emly` (repo separato)

Nell'equivalente client (`backend/utils/updateripc` lato `emly`, stesso
pattern speculare):
- costruisci `Envelope{ProtocolVersion, SenderVersion, Body: &Envelope_FooRequest{...}}`
- scrivi/leggi con lo stesso framing lunghezza-prefissata (`writeEnvelope`/
  `readEnvelope` equivalenti lato client)
- gestisci `Envelope_FooResponse` nella risposta e il case `Envelope_Error`
  per gli errori (`ERROR_CODE_*`)

### 5. Bump `ProtocolVersion`?

Aggiungere un nuovo message/campo al `oneof` è **wire-compatible** (un peer
vecchio ignora un campo che non conosce) — di norma **non serve** bump. Bump
`ProtocolVersion` (in `internal/ipc/server.go` qui e l'equivalente in `emly`)
solo se il cambio rompe la compatibilità wire (es. cambio di semantica di un
campo esistente, non solo aggiunta). Se bumpi:
- aggiorna la matrice di compatibilità in cima a `proto/updateripc.proto`
  (nuova riga con min/max)
- valuta se serve anche bump di `MinCompatible*Version`

### 6. Test

- `internal/ipc/server_test.go`: aggiungi caso per `FooRequest` →
  `FooResponse` atteso.
- `internal/ipc/framing_test.go`: tocca solo se cambi il framing (raro).

---

## Caso B — Nuovo campo su message esistente (es. campo in `SystemInfoResponse`)

1. Proto: aggiungi campo con numero libero, in entrambi i repo:
   ```proto
   message SystemInfoResponse {
     string hostname = 1;
     string hwid = 2;
     string internal_ip = 3;
     string os_version = 4;
     string new_field = 5;
   }
   ```
2. Rigenera `ipcpb` in entrambi i repo (comando sopra).
3. Popola il campo lato server in `dispatch()` (`internal/ipc/server.go`).
4. Consuma il campo lato client in `emly`.
5. Niente bump `ProtocolVersion` di norma (aggiunta campo = wire-compatible).
6. Aggiorna test esistenti che confrontano il payload atteso.

---

## Caso C — Nuovo `ErrorCode`

1. Proto: aggiungi variante a `enum ErrorCode` (numero libero), entrambi i repo.
2. Rigenera `ipcpb` in entrambi.
3. Usa il nuovo codice lato server con `errorEnvelope(ipcpb.ErrorCode_ERROR_CODE_..., "msg")`
   — messaggio **generico**, mai path/PID (vedi commento su `ErrorResponse.message`
   nel proto: mai echo di path o PID al client).
4. Gestisci il nuovo codice lato client in `emly` dove fa lo switch su
   `ErrorResponse.Code`.

---

## Checklist finale (ogni caso)

- [ ] `.proto` identico in `emly-updater` e `emly`
- [ ] `go generate ./internal/ipc/ipcpb` rieseguito in **entrambi** i repo
- [ ] Server (`emly-updater/internal/ipc/server.go`): case aggiunto in `dispatch()`
- [ ] Client (`emly`): invio richiesta + handling risposta/errore
- [ ] `ProtocolVersion` bumpato **solo** se wire-incompatibile, in entrambi i repo
- [ ] Matrice di compatibilità in `proto/updateripc.proto` aggiornata se bump
- [ ] Test aggiornati/aggiunti (`server_test.go`)
- [ ] Se release: bump `MaxCompatibleEMLyVersion` (qui) / `MaxCompatibleUpdaterVersion`
      (in `emly`) — vedi `internal/ipc/version.go` e AGENTS.md "Common Pitfalls"
