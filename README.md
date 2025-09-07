# indicer

### generate go files out of proto file

```sh
protoc -Iprotos --go_out=.\pb --go_opt=module=indicer/pb  --go-grpc_out=.\pb --go-grpc_opt=module=indicer/pb .\protos\dues.proto

```

## Functions and proto mapping

1. Upload file
- Flutter gets sha256 file hash - rpc call - go check file existence and completeness
- If not present or incomplete - rpc call client side stream bytes - go save complete file inot temp, dedup+index rm temp
- Return map error, map of chunk hash to chunk size

2. List all file objects
- For every file object return image_type, file_name, total_size, error, map of chunk hash to chunk size
- File information and chunk map will help in populating stats, additionally this information can be used to show compression of a single disk image and in between multiple disk images to show how much common data they share

3. Keyword search
- Runs global substring search in entire db and returns total count of keyword found, map of files and count of keyword found