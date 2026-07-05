# Compression Product API and ABI Reference

## Purpose

This document is the frontend integration reference for the compression product.
It covers:

- Native ABI exported by the Go FFI bridge
- JSON API request and response envelope
- Supported compression operations and their request and response shapes
- Behavior notes needed for UI and workflow design

## Component Overview

Call flow for frontend applications:

1. Frontend calls exported native function DuesDispatchJSON with a JSON request
   string.
2. Dispatcher routes to product compression_product.
3. Product operation executes and returns JSON response.
4. Frontend frees returned native string with DuesFreeString.

## Native ABI (FFI)

Source: cmd/ffi/main.go

### Exported functions

- char* DuesDispatchJSON(char* requestJSON)
- void DuesFreeString(char* ptr)

### ABI semantics

- DuesDispatchJSON input:
  - UTF-8 JSON request string
  - If null pointer is passed, backend uses {} as request payload
- DuesDispatchJSON output:
  - UTF-8 JSON response string allocated by Go C allocator bridge
- DuesFreeString:
  - Must be called by the caller for every non-null string returned by
    DuesDispatchJSON
  - Do not free returned pointers using any other allocator

### Memory ownership rules

- Request buffer ownership remains with caller.
- Response buffer ownership transfers to caller and must be released with
  DuesFreeString.

## JSON Envelope Contract

Source: internal/core/jsonbridge/jsonbridge.go

### Request envelope

{ "version": "v1", "product": "compression_product", "operation":
"operation_name", "params": {} }

### Response envelope (success)

{ "version": "v1", "status": "ok", "result": {} }

### Response envelope (error)

{ "version": "v1", "status": "error", "error": { "code": "error_code",
"message": "human_readable_message", "details": { "key": "value" } } }

### Validation rules

- version is required and must be v1
- product is required
- operation is required
- params defaults to {} if omitted/null

## Product Identity

- product name: compression_product

## Supported Operations

Source: internal/products/compression_product/service.go

- capabilities
- create_archive
- extract_archive
- list_archive
- verify_archive
- get_progress
- cancel_task
- compress_bytes
- decompress_bytes
- compress_file
- decompress_file

## Operation Reference

## 1) capabilities

Request params:

{}

Result shape:

{ "product": "compression_product", "operations": ["..."], "levels": ["fast",
"default", "best"] }

## 2) create_archive

Behavior:

- Uses DUES store pipeline for dedup plus compression.
- Packages input content into .duesarc archive.
- Can split archive into volume parts when split_size_mb is set.

Request params:

{ "input_path": "string, optional", "input_paths": ["string", "..."],
"output_path": "string, optional", "chunk_size_kb": "number, optional, default
256", "password": "string, optional", "split_size_mb": "number, optional",
"keep_work_dir": "boolean, optional", "async": "boolean, optional" }

Notes:

- Provide either input_path (single file or folder) or input_paths (multiple
  files).
- input_paths accepts files only; directory entries are rejected.
- If async=true, operation returns immediately with task_id and status=queued;
  poll get_progress to track completion and fetch result.

Result shape:

{ "input_path": "string", "input_paths": ["string", "..."], "output_path":
"string", "output_files": ["string"], "input_is_dir": true, "input_bytes": 123,
"archive_bytes": 123, "chunk_size_kb": 256, "split_size_mb": 0, "work_dir":
"string, optional" }

## 3) extract_archive

Behavior:

- Accepts single .duesarc file or first split volume part (.001).
- If split input is provided, service assembles parts before extraction.

Request params:

{ "input_path": "string, required", "output_path": "string, optional", "async":
"boolean, optional" }

Result shape:

{ "input_path": "string", "output_path": "string", "extracted_files": 10 }

## 4) list_archive

Behavior:

- Reads zip entries and returns metadata list.
- Supports split-volume input via internal assembly.

Request params:

{ "input_path": "string, required", "async": "boolean, optional" }

Result shape:

{ "input_path": "string", "entry_count": 10, "entries": [ { "name": "string",
"compressed_bytes": 123, "original_bytes": 456, "is_dir": false } ],
"has_manifest": true, "logical_file_count": 1, "logical_files": [ { "id":
"base64_id", "hash": "base64_hash", "name": "original/path/file.txt", "size":
1234, "completed": true, "failed": false, "status": "completed" } ] }

## 5) verify_archive

Behavior:

- Opens archive and streams all entries to validate readability.
- Verifies manifest.json presence and format.
- Verifies manifest db_path points to existing archive content.
- Supports split-volume input via internal assembly.

Request params:

{ "input_path": "string, required", "async": "boolean, optional" }

Result shape:

{ "input_path": "string", "valid": true, "entry_count": 10, "manifest_found":
true, "manifest_valid": true, "verified_bytes": 12345 }

## 6) get_progress

Behavior:

- Returns progress state for an async compression task started with async=true.
- Completed tasks include terminal result payload under result.
- Progress is byte-accurate for packaging, extraction, and verification copy
  loops.

Request params:

{ "task_id": "string, required" }

Result shape:

{ "task_id": "string", "operation":
"create_archive|extract_archive|list_archive|verify_archive|compress_file|decompress_file",
"status": "queued|running|completed|failed|canceled", "percent": 0, "stage":
"string, optional", "message": "string, optional", "error": "string, optional",
"result": { "...": "terminal success payload" } }

## 7) cancel_task

Behavior:

- Requests cancellation for an async task.
- Tasks already in terminal state return their current status with a message.
- Cancellation is propagated into ingest and archive I/O stages and is typically
  observed at chunk/copy boundaries.

Request params:

{ "task_id": "string, required" }

Result shape:

{ "task_id": "string", "status": "canceled|completed|failed", "message":
"string, optional" }

## 8) compress_bytes

Request params:

{ "data_base64": "string, required", "level": "fast | default | best, optional"
}

Result shape:

{ "original_bytes": 100, "compressed_bytes": 45, "data_base64": "string" }

## 9) decompress_bytes

Request params:

{ "data_base64": "string, required" }

Result shape:

{ "compressed_bytes": 45, "original_bytes": 100, "data_base64": "string" }

## 10) compress_file

Behavior:

- Input must be a file, not a directory.
- Routed to the same archive creation flow used by create_archive.
- Produces archive-style output metadata in fileRes shape.

Request params:

{ "input_path": "string, required", "output_path": "string, optional", "level":
"accepted but currently not used by store pipeline", "async": "boolean,
optional" }

Result shape:

{ "input_path": "string", "output_path": "string", "original_bytes": 123,
"compressed_bytes": 45 }

## 11) decompress_file

Behavior:

- Legacy file decompression path for zstd-compressed file bytes.
- For archive extraction, use extract_archive instead.

Request params:

{ "input_path": "string, required", "output_path": "string, optional", "async":
"boolean, optional" }

Result shape:

{ "input_path": "string", "output_path": "string", "original_bytes": 123,
"compressed_bytes": 45 }

## Error Handling Reference

Common validation and runtime error messages include:

- invalid params
- input_path is required
- invalid input_path
- input_path stat failed
- data_base64 is required
- data_base64 must be valid base64
- compression failed
- decompression failed
- archive extraction failed
- list archive failed
- verify archive failed
- manifest missing from archive
- manifest is invalid
- input_path must be a file
- input_paths only accepts files
- task_id is required
- progress task not found
- task is not cancelable
- task canceled

Frontend recommendation:

- Always display error.code and error.message.
- If error.details exists, render details in expandable UI for debugging.

## Frontend Integration Checklist

- Call DuesDispatchJSON with full envelope and product compression_product.
- Parse status field and branch success or error handling.
- Free every non-null response pointer with DuesFreeString.
- Prefer create_archive, extract_archive, list_archive, verify_archive as
  primary UX.
- For async operations, poll get_progress until status is completed or failed.
- Use cancel_task for user-initiated stop actions.
- Treat canceled as terminal and stop polling once status is canceled.
- async is supported for create_archive, extract_archive, list_archive,
  verify_archive, compress_file, and decompress_file.
- Treat decompress_file as legacy non-archive path.
- Support split-volume workflows by allowing .001 input selection.

## Flutter FFI Quick Example (DuesDispatchJSON)

Use this as the primary path for all compression product operations, including
list_archive, create_archive, extract_archive, verify_archive, compress_bytes,
and others.

```dart
import 'dart:convert';
import 'dart:ffi' as ffi;

import 'package:ffi/ffi.dart';

typedef _DuesDispatchJSONNative = ffi.Pointer<Utf8> Function(ffi.Pointer<Utf8>);
typedef _DuesDispatchJSONDart = ffi.Pointer<Utf8> Function(ffi.Pointer<Utf8>);

typedef _DuesFreeStringNative = ffi.Void Function(ffi.Pointer<Utf8>);
typedef _DuesFreeStringDart = void Function(ffi.Pointer<Utf8>);

class DuesDispatcherClient {
  DuesDispatcherClient(this._lib)
      : _dispatch = _lib.lookupFunction<_DuesDispatchJSONNative, _DuesDispatchJSONDart>('DuesDispatchJSON'),
        _freeString = _lib.lookupFunction<_DuesFreeStringNative, _DuesFreeStringDart>('DuesFreeString');

  final ffi.DynamicLibrary _lib;
  final _DuesDispatchJSONDart _dispatch;
  final _DuesFreeStringDart _freeString;

  Map<String, dynamic> callOperation({
    required String operation,
    Map<String, dynamic>? params,
  }) {
    final request = <String, dynamic>{
      'version': 'v1',
      'product': 'compression_product',
      'operation': operation,
      'params': params ?? <String, dynamic>{},
    };

    final requestPtr = jsonEncode(request).toNativeUtf8();
    final responsePtr = _dispatch(requestPtr);
    calloc.free(requestPtr);

    if (responsePtr == ffi.nullptr) {
      throw StateError('DuesDispatchJSON returned null pointer');
    }

    try {
      final responseJson = responsePtr.toDartString();
      return jsonDecode(responseJson) as Map<String, dynamic>;
    } finally {
      _freeString(responsePtr);
    }
  }
}
```

Example call:

```dart
final listRes = client.callOperation(
  operation: 'list_archive',
  params: {'input_path': 'C:/evidence/case.duesarc'},
);
if (listRes['status'] != 'ok') {
  throw Exception(listRes['error']?['message'] ?? 'Unknown error');
}

final res = client.callOperation(
  operation: 'verify_archive',
  params: {'input_path': 'C:/evidence/case.duesarc'},
);
if (res['status'] != 'ok') {
  throw Exception(res['error']?['message'] ?? 'Unknown error');
}

final startRes = client.callOperation(
  operation: 'create_archive',
  params: {
    'input_path': 'C:/evidence/folder',
    'output_path': 'C:/evidence/folder.duesarc',
    'async': true,
  },
);
final taskId = startRes['result']?['task_id'] as String;

while (true) {
  final p = client.callOperation(
    operation: 'get_progress',
    params: {'task_id': taskId},
  );
  final pRes = p['result'] as Map<String, dynamic>;
  final status = pRes['status'] as String;
  final percent = pRes['percent'] as num;
  if (status == 'completed') {
    break;
  }
  if (status == 'canceled') {
    throw Exception('Operation canceled by user');
  }
  if (status == 'failed') {
    throw Exception(pRes['error'] ?? 'Async operation failed');
  }
  await Future.delayed(const Duration(milliseconds: 500));
}

// Optional user cancellation
client.callOperation(
  operation: 'cancel_task',
  params: {'task_id': taskId},
);
```

## Versioning

- Current JSON API version: v1
- Product name must remain compression_product for operation routing.

## Minimal End-to-End Example

Request:

{ "version": "v1", "product": "compression_product", "operation":
"verify_archive", "params": { "input_path": "C:/evidence/case.duesarc" } }

Success response:

{ "version": "v1", "status": "ok", "result": { "input_path":
"C:/evidence/case.duesarc", "valid": true, "entry_count": 42, "manifest_found":
true, "manifest_valid": true, "verified_bytes": 1048576 } }
