## Simple collaborative text editor
The server receives edit operations, publishes them over Redis pub/sub, and all connected WebSocket clients can reconstruct the document by applying deltas.  

### WebSocket Endpoint
ws://localhost:8080/ws?docid=&user_name=

### edit operation
```
{
  "action": "edit", "doc_id":"1", "edit":{"edit_type":"insert", "index":0, "text":"test"}}
}
```

### sync request
```
{
  "action": "sync_req", "doc_id":"1"}
}
```
