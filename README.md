This is a simple web app consisting of two services: order and payment. Both services are instrumented and expose tracing information to the OpenTelemetry collector.

A detailed blog post will be published in the coming days.


### Requirements
Install mariadb
```bash
helm repo add bitnami https://charts.bitnami.com/bitnami
helm install mariadb bitnami/mariadb \
--set auth.rootPassword=secretpassword \
--set auth.username=appuser \
--set auth.password=secretpassword 
```

## Deploy the services
Via Helm

Services

```bash
helm install orderservice ./order/chart -f values-orderservice.yml
helm install paymentservice ./payment/chart -values-paymentservice.yml
```

Create an order

```bash
curl -X POST http://orderservice:8090/orders -H 'Content-Type: application/json' -d '{ "item": "t-shirt","amount": 100,"paid": false}'
curl http://orderservice:8090/orders/3/payment
```

Pay an order

```bash
curl -X POST http://paymentservice:8091/payments -H 'Content-Type: application/json' -d '{"order_id": 4,"amount": 100,"status": "pending"}'


```


## Generate Load
 
```bash
hey -n 10000  -m POST  -H 'Content-Type: application/json' -d '{ "item": "t-shirt","amount": 100,"paid": false}' http://orderservice.moin.rocks/orders

hey -n 1000  -m POST   -H 'Content-Type: application/json' -d '{"order_id": 4,"amount": 100,"status": "pending"}' http://paymentservice.moin.rocks/payments
```

### Generate Load

Generate Load
```bash
hey -n 1000000 -c 100  -m GET  http://orderservice.default:8090/orders/2
```


### Force out of bound in DB 500 Errors
```bash
hey -n 1000  -m POST   -H 'Content-Type: application/json' -d '{"order_id":444444445554,"amount": 100,"status": "pending"}' http://paymentservice.default:8091/payments
```