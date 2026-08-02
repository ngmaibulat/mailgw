import { Model, DataTypes } from "../../src/adapter.js";
import sequelize from "../config/sequelize.mjs";

export class Log extends Model {
    /**
     * Helper method for defining associations.
     * This method is not a part of Sequelize lifecycle.
     * The `models/index` file will call this method automatically.
     */
    static associate(models) {
        // define association here
    }
}

Log.init(
    {
        url: DataTypes.STRING,
        path: DataTypes.STRING,
        query: DataTypes.STRING,
        src_ip: DataTypes.STRING,
        src_port: DataTypes.INTEGER,
        referer: DataTypes.STRING,
        origin: DataTypes.STRING,
        method: DataTypes.STRING,
        user: DataTypes.STRING,
        userAgent: DataTypes.STRING,
    },
    {
        sequelize,
        modelName: "Log",
    }
);

export default Log;
