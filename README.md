# indicer

### generate go files out of proto file

```sh
protoc -Iprotos --go_out=.\pb --go_opt=module=indicer/pb  --go-grpc_out=.\pb --go-grpc_opt=module=indicer/pb .\protos\dues.proto

```