// payment_service.go

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jinzhu/gorm"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
)

type Payment struct {
	ID      uint   `json:"id" gorm:"primary_key"`
	OrderID uint   `json:"order_id"`
	Amount  int    `json:"amount"`
	Status  string `json:"status"`
}

const PORT = "8091"

var db *gorm.DB

func initTracer() (*sdktrace.TracerProvider, error) {
	otlp_endpoint := os.Getenv("OTLP_ENDPOINT")
	if otlp_endpoint == "" {
		log.Println("OTLP_ENDPOINT is not set, using tempo")
		otlp_endpoint = "tempo:4317"
	}
	exporter, err := otlptracegrpc.New(context.Background(), otlptracegrpc.WithEndpoint(otlp_endpoint), otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String("payment-service"),
		)),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp, nil
}

func main() {

	var err error
	os.Setenv("PORT", PORT)
	dbUser := os.Getenv("DB_USER")
	dbPassword := os.Getenv("DB_PASSWORD")
	dbName := os.Getenv("DB_NAME")
	dbHost := os.Getenv("DB_HOST")
	db, err = gorm.Open("mysql", dbUser+":"+dbPassword+"@tcp("+dbHost+":3306)/"+dbName+"?parseTime=True")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	// Auto-migrate the schema
	db.AutoMigrate(&Payment{})

	tp, err := initTracer()
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			panic(err)
		}
	}()

	// Init Gin router
	router := gin.Default()
	router.Use(func(c *gin.Context) {
		ctx := c.Request.Context()
		traceID := trace.SpanFromContext(ctx).SpanContext().TraceID()
		c.Set("traceID", traceID.String())
		c.Next()
	}, gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		return fmt.Sprintf("clientIP: %s Method: %s Path: %s Req: %s StatusCode %d Latency: %s Agent: %s traceID: %s\"\n",
			param.ClientIP,
			param.Method,
			param.Path,
			param.Request.Proto,
			param.StatusCode,
			param.Latency,
			param.Request.UserAgent(),
			param.Keys["traceID"].(string),
		)
	}))

	// Define routes
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))
	router.POST("/payments", processPayment)
	router.GET("/payments/:id", getPayment)
	if err := router.Run(); err != nil {
		panic(err)
	}
}

func processPayment(c *gin.Context) {
	tracer := otel.Tracer("payment-service")
	var payment Payment
	if err := c.BindJSON(&payment); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Process payment communication with payment gateway like stripe, paypal, etc.
	payment.Status = "success"
	// Start a new span for the database call
	ctx, dbSpan := tracer.Start(c.Request.Context(), "create payment in DB")
	defer dbSpan.End()

	// Create payment in database
	if err := db.Create(&payment).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		dbSpan.RecordError(err)
		dbSpan.SetStatus(codes.Error, "Failed to create payment in DB")
		return
	}

	dbSpan.SetAttributes(
		attribute.Int("order_id", int(payment.OrderID)),
		attribute.Int("amount", payment.Amount),
	)

	orderservice := os.Getenv("ORDER_SERVICE")
	if orderservice == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ORDER_SERVICE is not set"})
		return
	}

	reqBody, _ := json.Marshal(map[string]bool{"paid": true})
	orderURL := fmt.Sprintf("%s/orders/%d/pay", orderservice, payment.OrderID)
	req, err := http.NewRequestWithContext(ctx, "PUT", orderURL, bytes.NewBuffer(reqBody))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request to order service"})
		dbSpan.RecordError(err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	ctx, span := otel.Tracer("payment-service").Start(ctx, "update order service")
	// Start a new span for the HTTP request
	_, httpSpan := tracer.Start(ctx, "update order service")
	defer httpSpan.End()
	client := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to update order service")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update order service"})
		return
	}
	c.JSON(http.StatusCreated, payment)
}

func getPayment(c *gin.Context) {
	_, span := otel.Tracer("payment-service").Start(c.Request.Context(), "getPayment")
	defer span.End()
	var payment Payment
	id := c.Param("id")

	if err := db.First(&payment, id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	span.SetAttributes(
		attribute.String("payment_id", id),
	)
	c.JSON(http.StatusOK, payment)
}
