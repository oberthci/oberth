Historical restore chart fixtures

oberth-v0.22.4-chart.tgz is git archive of v0.22.4:charts/oberth.
oberth-v0.22.6-chart.tgz is git archive of b9dbcfa9f80bd84b9b290d493adc7c96232acb66:charts/oberth.
Archives retain exact source chart bytes, including unstamped Chart.yaml metadata.
The tests render locally with helm template; they never contact a cluster.
The v0.22.4 installer user-values fixture is restored against both historical targets and the current source chart.
The intermediate target retains execution settings but rejects Transit/profile settings; the old target retains all of them, and the current target rejects all of them.
