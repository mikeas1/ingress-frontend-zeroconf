FROM golang:1.13
WORKDIR /go/src/app
ADD . /go/src/app

RUN go get -d -v ./...

RUN go build -o /go/bin/app

CMD ["/go/bin/app", "--debug"]
