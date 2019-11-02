# Pull statistics from Tesla API into InfluxDB in Go

Project inspired by https://github.com/lephisto/tesla-apiscraper, completely rewritten in [Golang](https://golang.org) in flavor of flexibility in goroutines and better performance on my SBC (ODROID XU4).

InfluxDB scheme is compatible with the original project. The same Grafana Dashboards could be used to visualize the timeline.
