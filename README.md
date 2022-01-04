<h1 align="center">yadwh</h1>
<p align="center"><strong>Y</strong>et <strong>A</strong>nother <strong>D</strong>ocker <strong>W</strong>ebhook</p>


<img src="https://user-images.githubusercontent.com/71837281/148122013-aa3b92fd-d8b4-43eb-918a-b786a54f94b1.mov" align="center" />



---

This simple webhook service can be used to restart docker applications by calling a webhook URL,  
inspired by [Watchtower](https://github.com/containrrr/watchtower)

### Step 1
Add a label with the key `io.d2a.yadwh.ug` to your containers, you wish to restart, with a group-name, like `BACKEND_PROD`:
````yaml
# ...
services:
  backend:
    # ...
    labels:
      - "io.d2a.yadwh.ug=BACKEND_PROD"
````

### Step 2
Add an instance of yadwh to your `docker-compose.yml`, mount your Docker socket and expose the port `80`
```yaml
services:
  backend:
    # ...
  yadwh:
    image: darmiel/yadwh:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    ports:
      - "8080:80"
```

### Step 3
Choose a secret for your webhook my setting the environment variable `WH_SECRET_<NAME>`:
```yaml
services:
  backend:
  # ...
  yadwh:
    image: darmiel/yadwh:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    ports:
      - "8080:80"
    environment:
      WH_SECRET_BACKEND_PROD: mysecret
```
Done!  
Once the webhook is called, all containers with the label `io.d2a.yadwh.ug` set to `BACKEND_PROD` will be stopped, updated and started again.

## Example
### docker-compose.yml

```yaml
version: "3"

services:
  backend:
    image: ghcr.io/qwiri/gyf-backend:prod
    # ...
    labels:
      - "io.d2a.yadwh.ug=BACKEND_PROD"

  yadwh:
    image: darmiel/yadwh
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    restart: on-failure
    ports:
      - "8080:80"
    environment:
      WH_SECRET_BACKEND_PROD: mysecret
```

**GET** `X.X.X.X:8080/BACKEND_PROD/mysecret` would now restart the `backend`-service.

## ToDo
- [ ] Make a proper README
