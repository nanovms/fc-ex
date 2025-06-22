#!/bin/sh

curl --header "Content-Type: application/json" \
  --request POST \
  --data '{"root_image_path":"/home/eyberg/.ops/images/test"}' \
  http://localhost:8080/create
