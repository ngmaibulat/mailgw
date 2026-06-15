
let data = {
    name: "demo"
}

let jsondata = JSON.stringify(data);

let url = "http://localhost:3000/log";

let req = {
    method: 'POST',
    headers: {
        'Accept': 'application/json',
        'Content-Type': 'application/json'
    },
    body: jsondata
};

fetch(url, req).then(res => console.log(res));
