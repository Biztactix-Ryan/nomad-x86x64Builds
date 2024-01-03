# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0


job "foo" {
  task "bar" {
    driver = "docker"

    scaling "cpu" {
      enabled = true
      min     = 50
      max     = 1000

      policy {
        test = "cpu"
      }
    }

    scaling "cpu" {
      enabled = true
      min     = 50
      max     = 1000

      policy {
        test = "cpu"
      }
    }

  }
}

